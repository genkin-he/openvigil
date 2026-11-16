package server

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"vigil/internal/inbox"
)

// fakeSlack plays the watch: an instruction in the self-DM is answered in its thread at once.
type fakeSlack struct {
	mu      sync.Mutex
	threads map[string][]string
	n       int
	silent  bool // never answer (the watch is down)
}

var askReply = regexp.MustCompile(`回复「([^」]+)」`)

func (f *fakeSlack) Post(_ context.Context, _, text, thread string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	ts := "100." + strings.Repeat("1", f.n)
	if thread == "" {
		thread = ts
	}
	f.threads[thread] = append(f.threads[thread], text)
	if m := askReply.FindStringSubmatch(text); m != nil && !f.silent {
		f.threads[thread] = append(f.threads[thread], m[1])
	}
	return ts, nil
}

func (f *fakeSlack) Thread(_ context.Context, _, ts string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.threads[ts]...), nil
}

// actOnAnswers does what the watch does with an answered to-do: post the reply and close the item.
func actOnAnswers(s *Server, sl *fakeSlack, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Millisecond):
		}
		items, _ := s.Inbox.List(false)
		for _, it := range items {
			if it.Status == inbox.StatusAnswered {
				thread := it.Target[strings.LastIndex(it.Target, ":")+1:]
				_, _ = sl.Post(context.Background(), "D1", it.Answer, thread)
				_, _ = s.Inbox.Close(it.ID, it.Answer)
			}
		}
	}
}

func runSelfTest(t *testing.T, silent bool) (*Server, *SelfTestReport) {
	t.Helper()
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	sl := &fakeSlack{threads: map[string][]string{}, silent: silent}
	s.SelfTest = &SelfTest{Task: "slack", Channel: "D1", Slack: sl, Step: 2 * time.Second, Poll: 10 * time.Millisecond, Every: time.Hour,
		Path: t.TempDir() + "/selftest.json"}
	stop := make(chan struct{})
	defer close(stop)
	go actOnAnswers(s, sl, stop)

	if rec, _ := do(h, "POST", "/api/selftest", "", true); rec.Code != 200 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rep := s.SelfTest.Last()
		if rep.Status != "running" {
			return s, rep
		}
		if time.Now().After(deadline) {
			t.Fatalf("self-test still running: %+v", rep)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSelfTestPasses(t *testing.T) {
	s, rep := runSelfTest(t, false)
	if rep.Status != "pass" || len(rep.Steps) != 3 {
		t.Fatalf("report = %+v", rep)
	}
	// A pass right after another is skipped for automatic runs.
	if again, started := s.SelfTest.begin(s.Now(), true); started || again.ID != rep.ID {
		t.Fatal("automatic run should be skipped after a recent pass")
	}
	// The report survives a restart.
	fresh := &SelfTest{Path: s.SelfTest.Path, Every: time.Hour}
	if err := fresh.Load(); err != nil || fresh.Last() == nil || fresh.Last().ID != rep.ID {
		t.Fatalf("reloaded = %+v %v", fresh.Last(), err)
	}
}

func TestSelfTestFailureRaisesTodo(t *testing.T) {
	s, rep := runSelfTest(t, true)
	if rep.Status != "fail" || rep.Steps[0].OK {
		t.Fatalf("report = %+v", rep)
	}
	it, ok, _ := s.Inbox.OpenByRef(selfTestRef)
	if !ok || it.TaskID != inbox.SystemTask || !strings.Contains(it.Analysis, "线程里") {
		t.Fatalf("system to-do = %+v ok=%v", it, ok)
	}
}
