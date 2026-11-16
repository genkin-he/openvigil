// Package resident runs task turns in a long-lived agent hosted by herdr.
package resident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"vigil/internal/exec"
	"vigil/internal/herdr"
	"vigil/internal/result"
	"vigil/internal/task"
)

const (
	readLines      = 60
	settleQuiet    = 90 * time.Second // idle without a report for this long → fall back to the screen
	clearWait      = 60 * time.Second
	escalateWait   = 30 * time.Second
	maxDialogTries = 3
)

// Herdr is the subset of the herdr client the resident executor needs.
type Herdr interface {
	AgentGet(ctx context.Context, name string) (herdr.Agent, error)
	CreateWorkspace(ctx context.Context, cwd, label string) (string, error)
	StartAgent(ctx context.Context, name, kind, pane string, args []string) error
	Prompt(ctx context.Context, name, text string) error
	Read(ctx context.Context, name string, lines int) (string, error)
	SendKeys(ctx context.Context, name string, keys ...string) error
}

// Server starts the herdr session when it is down.
type Server interface {
	Running(ctx context.Context) bool
	StartServer() error
}

const serverStartWait = 15 * time.Second

// Dialog is a known interactive prompt and the keys that answer it safely.
type Dialog struct {
	Name  string
	Match string
	Keys  []string
}

// KnownDialogs are answered automatically. Only prompts whose safe answer is unambiguous belong here.
var KnownDialogs = []Dialog{
	{Name: "trust-folder", Match: "Yes, I trust this folder", Keys: []string{"down", "enter"}},
}

// Outcome is the result of one resident turn.
type Outcome struct {
	Result    *result.Result // reported through vigil done
	FinalText string         // screen text when no report arrived
	SessionID string
	TimedOut  bool
	Blocked   string // screen of an unknown dialog the owner must handle
	Err       error
}

// Executor runs turns. Poll is how often agent state is checked.
type Executor struct {
	H      Herdr
	Server Server // optional: lets Ensure bring a stopped session back
	Poll   time.Duration
	Sleep  func(time.Duration)
}

func (e Executor) sleep(d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Ensure makes sure the task's agent exists and is not stuck on a known dialog.
func (e Executor) Ensure(ctx context.Context, t task.Task) (herdr.Agent, error) {
	name := task.AgentName(t.ID)
	a, err := e.H.AgentGet(ctx, name)
	if herdr.IsCode(err, herdr.CodeServerDown) && e.Server != nil {
		if serr := e.startServer(ctx); serr != nil {
			return herdr.Agent{}, serr
		}
		a, err = e.H.AgentGet(ctx, name)
	}
	if err == nil {
		if a.Status == herdr.StatusBlocked {
			if _, err := e.resolveDialog(ctx, name); err != nil {
				return a, err
			}
		}
		return a, nil
	}
	if !errors.Is(err, herdr.ErrNoAgent) {
		return herdr.Agent{}, err
	}
	cwd := exec.ExpandHome(t.Agent.Cwd)
	if cwd == "" {
		cwd = exec.ExpandHome("~")
	}
	pane, err := e.H.CreateWorkspace(ctx, cwd, "Vigil · "+t.Name)
	if err != nil {
		return herdr.Agent{}, fmt.Errorf("create workspace: %w", err)
	}
	err = e.H.StartAgent(ctx, name, t.Agent.Kind, pane, t.Agent.Args)
	for i := 0; i < maxDialogTries && herdr.IsCode(err, herdr.CodeAgentNotReady); i++ {
		if _, derr := e.resolveDialog(ctx, name); derr != nil {
			return herdr.Agent{}, derr
		}
		e.sleep(3 * time.Second)
		var a herdr.Agent
		a, err = e.H.AgentGet(ctx, name)
		if err == nil && a.Status == herdr.StatusBlocked {
			err = &herdr.APIError{Code: herdr.CodeAgentNotReady, Message: "still blocked"}
		}
	}
	if err != nil {
		return herdr.Agent{}, fmt.Errorf("start agent %s: %w", name, err)
	}
	return e.H.AgentGet(ctx, name)
}

func (e Executor) startServer(ctx context.Context) error {
	if err := e.Server.StartServer(); err != nil {
		return fmt.Errorf("start herdr session: %w", err)
	}
	deadline := time.Now().Add(serverStartWait)
	for time.Now().Before(deadline) {
		if e.Server.Running(ctx) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("herdr session did not come up in time")
}

const (
	submitChecks   = 8
	submitInterval = time.Second
	pastedMarker   = "[Pasted text"
)

// Submit sends a prompt and makes sure it was actually submitted: a long paste can leave the text
// sitting in the input box, in which case Enter is pressed again.
func Submit(ctx context.Context, h Herdr, name, text string, sleep func(time.Duration)) error {
	if err := h.Prompt(ctx, name, text); err != nil {
		return err
	}
	for i := 0; i < submitChecks; i++ {
		sleep(submitInterval)
		a, err := h.AgentGet(ctx, name)
		if err != nil {
			return nil
		}
		if a.Status == herdr.StatusWorking || a.Status == herdr.StatusBlocked {
			return nil
		}
		if screen, err := h.Read(ctx, name, 12); err == nil && strings.Contains(screen, pastedMarker) {
			if err := h.SendKeys(ctx, name, "enter"); err != nil {
				return err
			}
		}
	}
	return nil
}

// UnknownDialogError carries the screen of a dialog Vigil will not answer on its own.
type UnknownDialogError struct{ Screen string }

func (u *UnknownDialogError) Error() string { return "agent is waiting on an unknown dialog" }

func (e Executor) resolveDialog(ctx context.Context, name string) (string, error) {
	screen, err := e.H.Read(ctx, name, readLines)
	if err != nil {
		return "", err
	}
	for _, d := range KnownDialogs {
		if strings.Contains(screen, d.Match) {
			return d.Name, e.H.SendKeys(ctx, name, d.Keys...)
		}
	}
	return "", &UnknownDialogError{Screen: tail(screen, 25)}
}

// Turn runs one prompt. reports delivers the result posted through `vigil done`.
func (e Executor) Turn(ctx context.Context, t task.Task, prompt string, rotate bool, reports <-chan result.Result) Outcome {
	name := task.AgentName(t.ID)
	a, err := e.Ensure(ctx, t)
	if err != nil {
		return blockedOrErr(err)
	}
	if rotate {
		if err := e.clear(ctx, name); err != nil {
			return Outcome{Err: err}
		}
	}
	if err := Submit(ctx, e.H, name, prompt, e.sleep); err != nil {
		if !herdr.IsCode(err, herdr.CodeAgentBlocked) {
			return Outcome{Err: fmt.Errorf("prompt: %w", err)}
		}
		if _, derr := e.resolveDialog(ctx, name); derr != nil {
			return blockedOrErr(derr)
		}
		if err := Submit(ctx, e.H, name, prompt, e.sleep); err != nil {
			return Outcome{Err: fmt.Errorf("prompt after dialog: %w", err)}
		}
	}
	out := e.wait(ctx, t, name, reports)
	if out.SessionID == "" {
		out.SessionID = a.SessionID()
	}
	return out
}

func blockedOrErr(err error) Outcome {
	var u *UnknownDialogError
	if errors.As(err, &u) {
		return Outcome{Blocked: u.Screen}
	}
	return Outcome{Err: err}
}

func (e Executor) clear(ctx context.Context, name string) error {
	if err := e.H.Prompt(ctx, name, "/clear"); err != nil {
		return fmt.Errorf("clear context: %w", err)
	}
	deadline := time.Now().Add(clearWait)
	for time.Now().Before(deadline) {
		e.sleep(e.Poll)
		if a, err := e.H.AgentGet(ctx, name); err == nil && a.Status != herdr.StatusWorking {
			return nil
		}
	}
	return nil
}

func (e Executor) wait(ctx context.Context, t task.Task, name string, reports <-chan result.Result) Outcome {
	deadline := time.Now().Add(t.Run.Timeout.Std())
	quietSince := time.Time{}
	ticker := time.NewTicker(e.Poll)
	defer ticker.Stop()
	for {
		select {
		case r := <-reports:
			return Outcome{Result: &r}
		case <-ctx.Done():
			return Outcome{Err: ctx.Err()}
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return e.interrupt(ctx, name)
		}
		a, err := e.H.AgentGet(ctx, name)
		if errors.Is(err, herdr.ErrNoAgent) {
			return Outcome{Err: errors.New("agent exited during the run")}
		}
		if err != nil {
			continue
		}
		switch a.Status {
		case herdr.StatusBlocked:
			if _, derr := e.resolveDialog(ctx, name); derr != nil {
				out := blockedOrErr(derr)
				out.SessionID = a.SessionID()
				return out
			}
			quietSince = time.Time{}
		case herdr.StatusWorking:
			quietSince = time.Time{}
		default:
			if quietSince.IsZero() {
				quietSince = time.Now()
			} else if time.Since(quietSince) >= settleQuiet {
				screen, _ := e.H.Read(ctx, name, readLines)
				return Outcome{FinalText: tail(screen, 40), SessionID: a.SessionID()}
			}
		}
	}
}

// interrupt stops a turn that ran past its timeout: Esc first, then Ctrl+C.
func (e Executor) interrupt(ctx context.Context, name string) Outcome {
	_ = e.H.SendKeys(ctx, name, "esc")
	e.sleep(escalateWait)
	if a, err := e.H.AgentGet(ctx, name); err == nil && a.Status == herdr.StatusWorking {
		_ = e.H.SendKeys(ctx, name, "ctrl+c")
	}
	screen, _ := e.H.Read(ctx, name, readLines)
	return Outcome{TimedOut: true, FinalText: tail(screen, 40)}
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	return strings.Join(kept, "\n")
}
