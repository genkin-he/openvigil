package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"vigil/internal/inbox"
	"vigil/internal/patch"
)

const sampleDiff = "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n"

// fakeLander records what was landed and installed.
type fakeLander struct {
	mu        sync.Mutex
	landErr   error
	landed    []string
	installed []string
}

func (f *fakeLander) land(_ context.Context, diffPath, msg string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := os.ReadFile(diffPath)
	f.landed = append(f.landed, string(data)+"|"+msg)
	return "abcdef1234567", f.landErr
}

func (f *fakeLander) install(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, id)
	return nil
}

func patchServer(t *testing.T) (*Server, http.Handler, *fakeLander) {
	t.Helper()
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: self-evolve\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	f := &fakeLander{}
	s.Patcher = &Patcher{Store: patch.Store{Root: t.TempDir()}, Land: f.land, Install: f.install}
	return s, h, f
}

func proposePatch(t *testing.T, h http.Handler) (patchID, todoID string) {
	t.Helper()
	body, _ := json.Marshal(patchRequest{Task: "self-evolve", Title: "收窄范围", Why: "误触发", Diff: sampleDiff})
	rec, env := do(h, "POST", "/api/patches", string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	data := env.Data.(map[string]any)
	return data["patch"].(map[string]any)["id"].(string), data["todo"].(string)
}

func TestAnApprovedPatchIsLandedInstalledAndClosed(t *testing.T) {
	s, h, f := patchServer(t)
	pid, tid := proposePatch(t, h)
	it, _ := s.Inbox.Get(tid)
	if it.Reply != sampleDiff || it.Ref != "patch:"+pid || it.Analysis != "误触发" {
		t.Fatalf("card = %+v", it)
	}

	if _, err := s.AnswerTodo(tid, TodoSend, "anything the page sent"); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if len(f.landed) != 1 || !strings.HasPrefix(f.landed[0], sampleDiff+"|") || !strings.Contains(f.landed[0], "收窄范围") {
		t.Fatalf("landed = %q (the stored diff must land, never text from the request)", f.landed)
	}
	if len(f.installed) != 1 || f.installed[0] != pid {
		t.Fatalf("installed = %v", f.installed)
	}
	if p, _ := s.Patcher.Store.Get(pid); p.Status != patch.StatusInstalling || p.Commit == "" {
		t.Fatalf("patch = %+v", p)
	}

	s.finishInstalls() // install still running: nothing changes
	if it, _ := s.Inbox.Get(tid); it.Status == inbox.StatusDone {
		t.Fatal("closed before the install finished")
	}
	if err := os.WriteFile(s.Patcher.Store.InstallExit(pid), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.finishInstalls()
	if it, _ := s.Inbox.Get(tid); it.Status != inbox.StatusDone || !strings.Contains(it.Outcome, "abcdef1") {
		t.Fatalf("after install: %+v", it)
	}
	if p, _ := s.Patcher.Store.Get(pid); p.Status != patch.StatusInstalled {
		t.Fatalf("patch = %+v", p)
	}
}

func TestAPatchThatFailsToLandComesBackWithTheReason(t *testing.T) {
	s, h, f := patchServer(t)
	f.landErr = errors.New("跑测试（make test）失败")
	_, tid := proposePatch(t, h)
	if _, err := s.AnswerTodo(tid, TodoSend, ""); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	it, _ := s.Inbox.Get(tid)
	if it.Status != inbox.StatusOpen || !strings.Contains(it.Analysis, "跑测试") || len(f.installed) != 0 {
		t.Fatalf("card = %+v installed=%v", it, f.installed)
	}
}

func TestAFailedInstallReopensTheCard(t *testing.T) {
	s, h, _ := patchServer(t)
	pid, tid := proposePatch(t, h)
	_, _ = s.AnswerTodo(tid, TodoSend, "")
	s.Wait()
	_ = os.WriteFile(s.Patcher.Store.InstallLog(pid), []byte("go: build failed"), 0o600)
	_ = os.WriteFile(s.Patcher.Store.InstallExit(pid), []byte("2"), 0o600)
	s.finishInstalls()
	it, _ := s.Inbox.Get(tid)
	if it.Status != inbox.StatusOpen || !strings.Contains(it.Analysis, "build failed") || !strings.Contains(it.Analysis, "旧版本") {
		t.Fatalf("card = %+v", it)
	}
}

func TestADeclinedPatchChangesNothing(t *testing.T) {
	s, h, f := patchServer(t)
	pid, tid := proposePatch(t, h)
	if _, err := s.AnswerTodo(tid, TodoDismiss, ""); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.Inbox.Get(tid); it.Status != inbox.StatusDone || it.Decision != inbox.DecisionDismissed {
		t.Fatalf("card = %+v", it)
	}
	if p, _ := s.Patcher.Store.Get(pid); p.Status != patch.StatusDeclined || len(f.landed) != 0 {
		t.Fatalf("patch = %+v landed=%v", p, f.landed)
	}
}

// Agent sends look like the owner in Slack, so Slack can turn a patch down but never approve it.
func TestAPatchCannotBeApprovedFromSlack(t *testing.T) {
	s, h, f := patchServer(t)
	_, tid := proposePatch(t, h)
	it, _ := s.Inbox.Get(tid)
	for _, yes := range []string{"发", "可以", "应用"} {
		if reply := s.applySlackAnswer(it, yes); !strings.Contains(reply, "控制台") {
			t.Fatalf("%s: reply = %q", yes, reply)
		}
	}
	s.Wait()
	if it, _ := s.Inbox.Get(tid); it.Status != inbox.StatusOpen || len(f.landed) != 0 {
		t.Fatalf("approved from Slack: %+v", it)
	}
	if _, err := s.AnswerTodo(tid, TodoInstruct, "改一下再应用"); err == nil {
		t.Fatal("a patch card took free-form instructions")
	}
	_ = s.applySlackAnswer(it, "不要")
	if it, _ := s.Inbox.Get(tid); it.Status != inbox.StatusDone {
		t.Fatalf("decline from Slack: %+v", it)
	}
}

func TestProposalsNeedASourceDir(t *testing.T) {
	s, h, _ := patchServer(t)
	s.Patcher = nil
	body, _ := json.Marshal(patchRequest{Task: "self-evolve", Title: "x", Diff: sampleDiff})
	if rec, _ := do(h, "POST", "/api/patches", string(body), true); rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}
