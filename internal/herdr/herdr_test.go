package herdr

import (
	"context"
	"errors"
	"slices"
	"testing"
)

type fake struct {
	calls  [][]string
	stdout string
	stderr string
	err    error
}

func (f *fake) run(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, args)
	return []byte(f.stdout), []byte(f.stderr), f.err
}

func TestAgentGetParsesAgent(t *testing.T) {
	f := &fake{stdout: `{"id":"x","result":{"agent":{"name":"v-a","agent":"claude","agent_status":"idle","pane_id":"w1:p1","workspace_id":"w1","agent_session":{"value":"sid"}}}}`}
	c := NewWithRunner("vigil", f.run)
	a, err := c.AgentGet(context.Background(), "v-a")
	if err != nil || a.Status != StatusIdle || a.SessionID() != "sid" || a.PaneID != "w1:p1" {
		t.Fatalf("AgentGet = %+v, %v", a, err)
	}
	if !slices.Equal(f.calls[0][:2], []string{"--session", "vigil"}) {
		t.Fatalf("session flag missing: %v", f.calls[0])
	}
}

func TestAgentGetNotFound(t *testing.T) {
	f := &fake{stderr: `{"error":{"code":"agent_not_found","message":"no agent"}}`, err: errors.New("exit 1")}
	_, err := NewWithRunner("vigil", f.run).AgentGet(context.Background(), "v-a")
	if !errors.Is(err, ErrNoAgent) {
		t.Fatalf("want ErrNoAgent, got %v", err)
	}
}

func TestStartAgentNotReadyIsTyped(t *testing.T) {
	f := &fake{stderr: `{"error":{"code":"agent_not_ready","message":"blocked"}}`, err: errors.New("exit 1")}
	err := NewWithRunner("vigil", f.run).StartAgent(context.Background(), "v-a", "claude", "w1:p1", []string{"--resume", "s"})
	if !IsCode(err, CodeAgentNotReady) {
		t.Fatalf("want agent_not_ready, got %v", err)
	}
	if !slices.Contains(f.calls[0], "--") || f.calls[0][len(f.calls[0])-1] != "s" {
		t.Fatalf("agent args not passed after --: %v", f.calls[0])
	}
}

func TestDefaultSessionHasNoFlag(t *testing.T) {
	f := &fake{stdout: `{"result":{"root_pane":{"pane_id":"w2:p1"}}}`}
	pane, err := NewWithRunner("default", f.run).CreateWorkspace(context.Background(), "/tmp", "x")
	if err != nil || pane != "w2:p1" || f.calls[0][0] != "workspace" {
		t.Fatalf("CreateWorkspace = %q, %v, %v", pane, err, f.calls)
	}
}

func TestPaneRunAcceptsEmptyOutput(t *testing.T) {
	f := &fake{}
	if err := NewWithRunner("default", f.run).PaneRun(context.Background(), "w1:p1", "echo hi"); err != nil {
		t.Fatalf("PaneRun: %v", err)
	}
}

func TestListsAndSimpleCalls(t *testing.T) {
	f := &fake{stdout: `{"result":{"agents":[{"name":"v-a","agent_status":"idle"}],"workspaces":[{"workspace_id":"w1","label":"x"}]}}`}
	c := NewWithRunner("vigil", f.run)
	ctx := context.Background()
	agents, err := c.AgentList(ctx)
	if err != nil || len(agents) != 1 || agents[0].Name != "v-a" {
		t.Fatalf("AgentList = %+v, %v", agents, err)
	}
	ws, err := c.WorkspaceList(ctx)
	if err != nil || len(ws) != 1 || ws[0].ID != "w1" {
		t.Fatalf("WorkspaceList = %+v, %v", ws, err)
	}
	for name, call := range map[string]func() error{
		"prompt": func() error { return c.Prompt(ctx, "v-a", "hi") },
		"keys":   func() error { return c.SendKeys(ctx, "v-a", "esc", "enter") },
		"focus":  func() error { return c.FocusWorkspace(ctx, "w1") },
		"close":  func() error { return c.CloseWorkspace(ctx, "w1") },
	} {
		if err := call(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	found := false
	for _, call := range f.calls {
		if slices.Contains(call, "send-keys") {
			found = slices.Equal(call[len(call)-2:], []string{"esc", "enter"})
		}
	}
	if !found {
		t.Fatalf("send-keys args not passed in order: %v", f.calls)
	}
}

func TestReadAndRunning(t *testing.T) {
	f := &fake{stdout: "line1\nline2\n"}
	c := NewWithRunner("vigil", f.run)
	out, err := c.Read(context.Background(), "v-a", 40)
	if err != nil || out != "line1\nline2\n" || !slices.Contains(f.calls[0], "recent-unwrapped") {
		t.Fatalf("Read = %q, %v, %v", out, err, f.calls[0])
	}
	if c.Running(context.Background()) {
		t.Fatal("Running should be false without the status marker")
	}
	f.stdout = "server:\n  status: running\n"
	if !c.Running(context.Background()) {
		t.Fatal("Running should be true")
	}
}

func TestPlainErrorIsWrapped(t *testing.T) {
	f := &fake{stderr: "boom", err: errors.New("exit 2")}
	if err := NewWithRunner("vigil", f.run).Prompt(context.Background(), "v-a", "x"); err == nil || IsCode(err, CodeAgentBlocked) {
		t.Fatalf("err = %v", err)
	}
	if !IsCode(&APIError{Code: CodeAgentBlocked}, CodeAgentBlocked) {
		t.Fatal("IsCode")
	}
}
