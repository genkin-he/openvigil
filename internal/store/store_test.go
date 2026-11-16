package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/task"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func manualTask(id string) task.Task {
	return task.Task{ID: id, Trigger: task.Trigger{Kind: task.TriggerManual}}
}

func TestPutGetList(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"b", "a"} {
		if err := s.Put(manualTask(id)); err != nil {
			t.Fatal(err)
		}
	}
	got, errs := s.List()
	if len(errs) != 0 || len(got) != 2 || got[0].ID != "a" {
		t.Fatalf("List = %+v, %v", got, errs)
	}
	if got[0].Agent.Kind != "claude" {
		t.Fatalf("defaults not stored: %+v", got[0])
	}
}

func TestGetMissingAndInvalid(t *testing.T) {
	s := newStore(t)
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.Get("../x"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestListReportsBrokenTask(t *testing.T) {
	s := newStore(t)
	if err := s.Put(manualTask("ok")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(s.Root(), "tasks", "bad")
	_ = os.MkdirAll(bad, 0o700)
	_ = os.WriteFile(filepath.Join(bad, "task.yaml"), []byte("id: bad\ntrigger: {kind: nope}\n"), 0o600)
	got, errs := s.List()
	if len(got) != 1 || len(errs) != 1 {
		t.Fatalf("got %d tasks, %d errs", len(got), len(errs))
	}
}

func TestRunbookHistoryAndProposals(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	_ = s.WriteDoc("a", DocRunbook, "v1")
	_ = s.WriteDoc("a", DocRunbook, "v2")
	hist, _ := os.ReadDir(filepath.Join(s.Root(), "tasks", "a", "history"))
	if len(hist) != 1 {
		t.Fatalf("want 1 archived runbook, got %d", len(hist))
	}
	name, err := s.SaveProposal("a", "v3", "更快", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Proposals("a")
	if len(ps) != 1 || ps[0].Reason != "更快" || ps[0].Content != "v3" {
		t.Fatalf("proposals = %+v", ps)
	}
	if err := s.ResolveProposal("a", name, true); err != nil {
		t.Fatal(err)
	}
	if rb, _ := s.ReadDoc("a", DocRunbook); rb != "v3" {
		t.Fatalf("runbook = %q", rb)
	}
	if err := s.ResolveProposal("a", "../x", true); err == nil {
		t.Fatal("bad proposal name accepted")
	}
}

func TestRunsLatestVersionNewestFirst(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	now := time.Now()
	_ = s.AppendRun(RunRecord{ID: "r1", TaskID: "a", Status: StatusRunning, StartedAt: now})
	_ = s.AppendRun(RunRecord{ID: "r2", TaskID: "a", Status: StatusRunning, StartedAt: now})
	_ = s.AppendRun(RunRecord{ID: "r1", TaskID: "a", Status: StatusOK, StartedAt: now})
	runs, err := s.Runs("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "r2" || runs[1].Status != StatusOK {
		t.Fatalf("runs = %+v", runs)
	}
	if one, _ := s.Runs("a", 1); len(one) != 1 {
		t.Fatalf("limit ignored: %d", len(one))
	}
}

func TestStateAndMemory(t *testing.T) {
	s := newStore(t)
	_ = s.Put(manualTask("a"))
	st, err := s.UpdateState("a", func(st State) State { st.FailStreak++; return st })
	if err != nil || st.FailStreak != 1 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	again, _ := s.State("a")
	if again.FailStreak != 1 {
		t.Fatalf("state not persisted: %+v", again)
	}
	_ = s.AppendMemory("a", []string{"记住 A", " ", "多行\n合并"}, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))
	_ = s.AppendMemory("a", []string{"2026-11-09 自带日期"}, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC))
	mem, _ := s.ReadDoc("a", DocMemory)
	if strings.Count(mem, "\n") != 3 || !strings.Contains(mem, "2026-11-10 多行 合并") || strings.Contains(mem, "2026-11-10 2026-11-09") {
		t.Fatalf("memory = %q", mem)
	}
}

func TestRemoveMemoryDropsMatchingLinesOnly(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(task.WithDefaults(task.Task{ID: "a", Name: "a", Trigger: task.Trigger{Kind: task.TriggerManual}})); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	if err := s.AppendMemory("a", []string{"我已授权：「问进度」类直接回", "课程接口归 misc-service"}, at); err != nil {
		t.Fatal(err)
	}
	n, err := s.RemoveMemory("a", "我已授权：「问进度」类直接回")
	if err != nil || n != 1 {
		t.Fatalf("removed %d, err %v", n, err)
	}
	mem, _ := s.ReadDoc("a", DocMemory)
	if strings.Contains(mem, "问进度") || !strings.Contains(mem, "misc-service") {
		t.Fatalf("memory = %q", mem)
	}
	// Only an exact note goes: a fragment of a line must not delete it, a dated copy of the line works.
	if err := s.AppendMemory("a", []string{"不要自动回复群聊", "群聊"}, at); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RemoveMemory("a", "- 2026-11-11 群聊"); err != nil || n != 1 {
		t.Fatalf("dated exact: %d %v", n, err)
	}
	if mem, _ := s.ReadDoc("a", DocMemory); !strings.Contains(mem, "不要自动回复群聊") {
		t.Fatalf("a longer line went too: %q", mem)
	}
	if _, err := s.RemoveMemory("a", "不存在的"); !errors.Is(err, ErrNoMemoryLine) {
		t.Fatalf("err = %v", err)
	}
}

func TestOwnerPrinciplesAppendAndReplaceKeepHistory(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadOwner(); err != nil || got != "" {
		t.Fatalf("empty owner doc = %q, %v", got, err)
	}
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	if err := s.AppendOwner("代回不替我许诺时间\n〔依据 3 次〕", at); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ReadOwner()
	if got != "- 2026-11-12 代回不替我许诺时间 〔依据 3 次〕\n" {
		t.Fatalf("after append = %q", got)
	}
	if err := s.WriteOwner("- 新的一份\n"); err != nil {
		t.Fatal(err)
	}
	old, _ := filepath.Glob(filepath.Join(s.Root(), "history", "owner.*.md"))
	if len(old) != 1 {
		t.Fatalf("previous version not kept: %v", old)
	}
	if s.OwnerPath() != filepath.Join(s.Root(), "owner.md") {
		t.Fatalf("path = %s", s.OwnerPath())
	}
}
