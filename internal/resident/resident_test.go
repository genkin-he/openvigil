package resident

import (
	"context"
	"sync"
	"testing"
	"time"

	"vigil/internal/herdr"
	"vigil/internal/result"
	"vigil/internal/task"
)

type fakeHerdr struct {
	mu        sync.Mutex
	exists    bool
	statuses  []string // returned in order by AgentGet, last one repeats
	screen    string
	startErr  error
	keys      [][]string
	prompts   []string
	created   int
	startArgs []string
}

func (f *fakeHerdr) AgentGet(context.Context, string) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return herdr.Agent{}, herdr.ErrNoAgent
	}
	st := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return herdr.Agent{Status: st}, nil
}

func (f *fakeHerdr) CreateWorkspace(context.Context, string, string) (string, error) {
	f.created++
	return "w1:p1", nil
}

func (f *fakeHerdr) StartAgent(_ context.Context, _, _, _ string, args []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists = true
	f.startArgs = args
	err := f.startErr
	f.startErr = nil
	return err
}

func (f *fakeHerdr) Prompt(_ context.Context, _, text string) error {
	f.prompts = append(f.prompts, text)
	return nil
}

func (f *fakeHerdr) Read(context.Context, string, int) (string, error) { return f.screen, nil }

func (f *fakeHerdr) SendKeys(_ context.Context, _ string, keys ...string) error {
	f.keys = append(f.keys, keys)
	return nil
}

func residentTask() task.Task {
	tk := task.WithDefaults(task.Task{ID: "watch", Mode: task.ModeResident, Trigger: task.Trigger{Kind: task.TriggerManual}})
	tk.Run.Timeout = task.Duration(time.Minute)
	return tk
}

func exe(f *fakeHerdr) Executor {
	return Executor{H: f, Poll: 5 * time.Millisecond, Sleep: func(time.Duration) {}}
}

func TestEnsureCreatesAgentAndAnswersTrustDialog(t *testing.T) {
	f := &fakeHerdr{statuses: []string{herdr.StatusIdle}, screen: "❯ No, exit\n  Yes, I trust this folder",
		startErr: &herdr.APIError{Code: herdr.CodeAgentNotReady}}
	if _, err := exe(f).Ensure(context.Background(), residentTask()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if f.created != 1 || len(f.keys) != 1 || f.keys[0][0] != "down" {
		t.Fatalf("created=%d keys=%v", f.created, f.keys)
	}
}

func TestEnsureRefusesUnknownDialog(t *testing.T) {
	f := &fakeHerdr{statuses: []string{herdr.StatusBlocked}, screen: "Allow rm -rf /? (y/n)",
		startErr: &herdr.APIError{Code: herdr.CodeAgentNotReady}}
	out := exe(f).Turn(context.Background(), residentTask(), "go", false, nil)
	if out.Blocked == "" || len(f.keys) != 0 {
		t.Fatalf("unknown dialog must not be answered: %+v keys=%v", out, f.keys)
	}
}

func TestTurnReturnsReportedResult(t *testing.T) {
	f := &fakeHerdr{exists: true, statuses: []string{herdr.StatusIdle, herdr.StatusIdle, herdr.StatusWorking}}
	reports := make(chan result.Result, 1)
	reports <- result.Result{Status: result.OK, Summary: "好"}
	out := exe(f).Turn(context.Background(), residentTask(), "go", true, reports)
	if out.Result == nil || out.Result.Summary != "好" {
		t.Fatalf("Turn = %+v", out)
	}
	if len(f.prompts) != 2 || f.prompts[0] != "/clear" {
		t.Fatalf("rotate should /clear first: %v", f.prompts)
	}
}

func TestTurnTimeoutInterrupts(t *testing.T) {
	f := &fakeHerdr{exists: true, statuses: []string{herdr.StatusWorking}}
	tk := residentTask()
	tk.Run.Timeout = task.Duration(20 * time.Millisecond)
	out := exe(f).Turn(context.Background(), tk, "go", false, nil)
	if !out.TimedOut || len(f.keys) != 2 || f.keys[0][0] != "esc" || f.keys[1][0] != "ctrl+c" {
		t.Fatalf("Turn = %+v keys=%v", out, f.keys)
	}
}

func TestTurnAgentExited(t *testing.T) {
	f := &fakeHerdr{exists: true, statuses: []string{herdr.StatusWorking}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.mu.Lock()
		f.exists = false
		f.mu.Unlock()
	}()
	out := exe(f).Turn(context.Background(), residentTask(), "go", false, nil)
	if out.Err == nil {
		t.Fatalf("want error when agent exits, got %+v", out)
	}
}

type fakeServer struct{ started bool }

func (f *fakeServer) Running(context.Context) bool { return f.started }
func (f *fakeServer) StartServer() error           { f.started = true; return nil }

type downHerdr struct {
	*fakeHerdr
	srv *fakeServer
}

func (d downHerdr) AgentGet(ctx context.Context, n string) (herdr.Agent, error) {
	if !d.srv.started {
		return herdr.Agent{}, &herdr.APIError{Code: herdr.CodeServerDown}
	}
	return d.fakeHerdr.AgentGet(ctx, n)
}

func TestEnsureStartsStoppedSession(t *testing.T) {
	srv := &fakeServer{}
	f := &fakeHerdr{statuses: []string{herdr.StatusIdle}}
	e := Executor{H: downHerdr{f, srv}, Server: srv, Poll: time.Millisecond, Sleep: func(time.Duration) {}}
	if _, err := e.Ensure(context.Background(), residentTask()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !srv.started || f.created != 1 {
		t.Fatalf("started=%v created=%d", srv.started, f.created)
	}
}

func TestSubmitPressesEnterWhenPasteIsStuck(t *testing.T) {
	f := &fakeHerdr{exists: true, statuses: []string{herdr.StatusIdle, herdr.StatusWorking}, screen: "❯ [Pasted text #1 +43 lines]"}
	if err := Submit(context.Background(), f, "x", "long", func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || f.keys[0][0] != "enter" {
		t.Fatalf("keys = %v", f.keys)
	}
}
