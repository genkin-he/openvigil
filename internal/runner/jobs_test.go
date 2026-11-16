package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"vigil/internal/exec"
	"vigil/internal/store"
	"vigil/internal/task"
)

// jobGate blocks background job runs until released; main runs pass straight through.
type jobGate struct {
	release chan struct{}
	started chan string
}

func (g *jobGate) Run(_ context.Context, req exec.Request) exec.Outcome {
	if strings.Contains(req.Prompt, "【Vigil 后台子任务】") {
		g.started <- req.Prompt
		<-g.release
	}
	return exec.Outcome{FinalText: okBlock, SessionID: "s-" + req.Task.ID}
}

func TestBackgroundJobDoesNotBlockMainRuns(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := &jobGate{release: make(chan struct{}), started: make(chan string, 1)}
	e := New(context.Background(), Config{Store: s, Headless: g, MaxConcurrent: 1, MaxJobs: 1})
	tk := task.WithDefaults(manual("slack"))
	tk.Mode = task.ModeResident // jobs always run headless, whatever the task's mode
	tk.Session.Mode = task.SessionContinue
	if err := s.Put(tk); err != nil {
		t.Fatal(err)
	}

	e.Submit(Request{TaskID: "slack", Trigger: ByJob, Input: "排查 24990896", Job: "j-aaaaaa", JobTitle: "排查缺陷"})
	jobPrompt := <-g.started
	if !strings.Contains(jobPrompt, "排查缺陷") || !strings.Contains(jobPrompt, "排查 24990896") || strings.Contains(jobPrompt, " spawn ") {
		t.Fatalf("job prompt:\n%s", jobPrompt)
	}
	if jobs := e.Jobs("slack"); len(jobs) != 1 || jobs[0].Job != "j-aaaaaa" {
		t.Fatalf("jobs in progress = %+v", jobs)
	}
	if e.Busy("slack") || !e.Active("slack") {
		t.Fatal("a job must not mark the main lane busy, but the task is active")
	}

	// A main run (here headless) completes while the job is still running, even with MaxConcurrent 1.
	main := task.WithDefaults(manual("cms"))
	if err := s.Put(main); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		e.Submit(Request{TaskID: "cms", Trigger: ByManual})
		for e.Busy("cms") {
			time.Sleep(5 * time.Millisecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("main run was blocked by a background job")
	}

	close(g.release)
	e.Wait()
	runs, _ := s.Runs("slack", 0)
	if len(runs) != 1 || runs[0].Trigger != ByJob || runs[0].Job != "j-aaaaaa" || runs[0].Status != store.StatusOK {
		t.Fatalf("job run = %+v", runs)
	}
	if st, _ := s.State("slack"); st.SessionID != "" || st.RunCount != 0 {
		t.Fatalf("a job must not touch the task's own session or counters: %+v", st)
	}
}

func TestRecoverReRunsInterruptedJobs(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(task.WithDefaults(manual("slack"))); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = s.AppendRun(store.RunRecord{ID: "r1", TaskID: "slack", Trigger: ByJob, Status: store.StatusRunning, Input: "修 bug", Job: "j-bbbbbb", JobTitle: "修 bug", StartedAt: now})
	redo, err := RecoverInterrupted(s, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(redo) != 1 || redo[0].Job != "j-bbbbbb" || redo[0].Input != "修 bug" {
		t.Fatalf("redo = %+v (the main lane must not be re-run for a job)", redo)
	}
	runs, _ := s.Runs("slack", 0)
	if runs[0].Status != store.StatusFail {
		t.Fatalf("interrupted job record = %+v", runs[0])
	}
}
