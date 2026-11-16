package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vigil/internal/exec"
	"vigil/internal/runner"
	"vigil/internal/store"
	"vigil/internal/stream"
	"vigil/internal/task"
)

type countHeadless struct {
	mu    sync.Mutex
	calls int
}

func (c *countHeadless) Run(context.Context, exec.Request) exec.Outcome {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return exec.Outcome{FinalText: "```json\n{\"status\":\"ok\",\"summary\":\"ok\"}\n```"}
}

func setup(t *testing.T) (*store.Store, *runner.Engine, *countHeadless) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &countHeadless{}
	e := runner.New(context.Background(), runner.Config{Store: s, Headless: h})
	return s, e, h
}

func nolog(string, ...any) {}

func TestTickBaselineThenRunsAndCatchesUp(t *testing.T) {
	s, e, h := setup(t)
	tk := task.WithDefaults(task.Task{ID: "h", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerEvery, Every: task.Duration(time.Hour)}})
	if err := s.Put(tk); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	Tick(s, e, t0, time.Time{}, nolog)
	e.Wait()
	if h.calls != 0 {
		t.Fatal("first tick must only set the baseline")
	}
	Tick(s, e, t0.Add(30*time.Minute), t0, nolog)
	e.Wait()
	if h.calls != 0 {
		t.Fatal("ran early")
	}
	woke := Tick(s, e, t0.Add(5*time.Hour+30*time.Minute), t0.Add(30*time.Minute), nolog)
	e.Wait()
	runs, _ := s.Runs("h", 0)
	if !woke || h.calls != 1 || runs[0].Trigger != runner.ByCatchUp {
		t.Fatalf("woke=%v calls=%d runs=%+v", woke, h.calls, runs)
	}
}

func TestTickOnceRunsOnlyOnce(t *testing.T) {
	s, e, h := setup(t)
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local)
	_ = s.Put(task.WithDefaults(task.Task{ID: "o", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerOnce, At: at}}))
	Tick(s, e, at.Add(time.Second), at, nolog)
	e.Wait()
	Tick(s, e, at.Add(time.Minute), at, nolog)
	e.Wait()
	if h.calls != 1 {
		t.Fatalf("calls = %d", h.calls)
	}
}

func TestStreamsSpoolAndRun(t *testing.T) {
	s, e, h := setup(t)
	tk := task.WithDefaults(task.Task{ID: "w", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerStream,
		Stream: &task.Stream{Command: "echo 'LISTENER connected {}'; echo one; echo two; sleep 30", Ignore: []string{"^LISTENER connected "}, BatchWindow: task.Duration(50 * time.Millisecond)}}})
	_ = s.Put(tk)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Streams{Store: s, Engine: e, Logf: nolog}
	m.Sync(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := h.calls
		h.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.Wait()
	dir, _ := s.SpoolDir("w")
	sp, _ := stream.OpenSpool(dir)
	left, _ := sp.Pending(0)
	if h.calls == 0 || len(left) != 0 {
		t.Fatalf("calls=%d pending=%d", h.calls, len(left))
	}
	delivered, _ := os.ReadFile(filepath.Join(dir, "delivered.jsonl"))
	if strings.Contains(string(delivered), "LISTENER") || !strings.Contains(string(delivered), "two") {
		t.Fatalf("ignored line was spooled or real lines missing: %s", delivered)
	}
	tk.Enabled = false
	_ = s.Put(tk)
	m.Sync(ctx)
	if len(m.running) != 0 {
		t.Fatal("disabled stream still running")
	}
}
