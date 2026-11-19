package forge

import (
	"context"
	"strings"
	"testing"
	"time"

	"vigil/internal/herdr"
	"vigil/internal/task"
)

type fakeHerdr struct {
	workspaces []herdr.Workspace
	runs       []string
	focused    []string
	closed     []string
	agents     map[string]bool
	// focusedAtStart is how many focuses had happened when the agent was started.
	focusedAtStart int
	promptErr      error
	screen         string
	keys           [][]string
}

func (f *fakeHerdr) AgentGet(_ context.Context, name string) (herdr.Agent, error) {
	if f.agents[name] {
		return herdr.Agent{Name: name, Status: herdr.StatusIdle, WorkspaceID: "w9"}, nil
	}
	return herdr.Agent{}, herdr.ErrNoAgent
}
func (f *fakeHerdr) AgentList(context.Context) ([]herdr.Agent, error) { return nil, nil }
func (f *fakeHerdr) CreateWorkspace(_ context.Context, _, label string) (string, error) {
	f.workspaces = append(f.workspaces, herdr.Workspace{ID: "w9", Label: label})
	return "w9:p1", nil
}
func (f *fakeHerdr) StartAgent(_ context.Context, name, _, _ string, _ []string) error {
	if f.agents == nil {
		f.agents = map[string]bool{}
	}
	f.agents[name] = true
	f.focusedAtStart = len(f.focused)
	return nil
}
func (f *fakeHerdr) Prompt(context.Context, string, string) error      { return f.promptErr }
func (f *fakeHerdr) Read(context.Context, string, int) (string, error) { return f.screen, nil }
func (f *fakeHerdr) SendKeys(_ context.Context, _ string, keys ...string) error {
	f.keys = append(f.keys, keys)
	return nil
}
func (f *fakeHerdr) WorkspaceList(context.Context) ([]herdr.Workspace, error) {
	return f.workspaces, nil
}
func (f *fakeHerdr) CloseWorkspace(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	kept := f.workspaces[:0]
	for _, w := range f.workspaces {
		if w.ID != id {
			kept = append(kept, w)
		}
	}
	f.workspaces = kept
	return nil
}
func (f *fakeHerdr) FocusWorkspace(_ context.Context, id string) error {
	f.focused = append(f.focused, id)
	return nil
}
func (f *fakeHerdr) PaneRun(_ context.Context, _ string, command string) error {
	f.runs = append(f.runs, command)
	return nil
}

func residentTask() task.Task {
	return task.WithDefaults(task.Task{ID: "slack-watch", Name: "Slack 监听", Mode: task.ModeResident, Trigger: task.Trigger{Kind: task.TriggerManual}})
}

func TestOpenResidentReplacesPreviousAttach(t *testing.T) {
	f := &fakeHerdr{workspaces: []herdr.Workspace{
		{ID: "w1", Label: "Vigil 常驻 · Slack 实时监听（websocket 常驻）"}, // pre-rename, matched by old label? no: different name
		{ID: "w2", Label: "Vigil 常驻 · 旧名字 · slack-watch"},
		{ID: "w3", Label: "blog"},
	}}
	fg := Forge{H: f, HerdrBin: "/bin/herdr", ResidentSession: "vigil"}
	if err := fg.OpenResident(context.Background(), residentTask()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 || f.closed[0] != "w2" {
		t.Fatalf("closed = %v", f.closed)
	}
	if len(f.runs) != 1 || f.runs[0] != "'/bin/herdr' --session 'vigil' agent attach v-slack-watch --takeover" {
		t.Fatalf("runs = %q", f.runs)
	}
	if len(f.focused) != 1 || f.focused[0] != "w9" {
		t.Fatalf("new workspace not focused: %v", f.focused)
	}
	last := f.workspaces[len(f.workspaces)-1]
	if last.Label != "Vigil 常驻 · Slack 监听 · slack-watch" {
		t.Fatalf("label = %q", last.Label)
	}
}

func TestOpenHeadlessNeedsSession(t *testing.T) {
	fg := Forge{H: &fakeHerdr{}}
	tk := residentTask()
	tk.Mode = task.ModeHeadless
	shown := 0
	show := func() { shown++ }
	if _, err := fg.Open(context.Background(), tk, "", show); err == nil || !strings.Contains(err.Error(), "没有可以接管的会话") {
		t.Fatalf("err = %v", err)
	}
	name, err := fg.Open(context.Background(), tk, "sid-1", show)
	if err != nil || name != "vo-slack-watch" || !fg.Held(context.Background(), "slack-watch") {
		t.Fatalf("open = %q, %v", name, err)
	}
	f := fg.H.(*fakeHerdr)
	if len(f.focused) != 1 || f.focused[0] != "w9" {
		t.Fatalf("takeover workspace not focused: %v", f.focused)
	}
	if f.focusedAtStart != 1 || shown != 1 {
		t.Fatalf("workspace should be shown before claude boots: focusedAtStart=%d shown=%d", f.focusedAtStart, shown)
	}
	// Taking over again just brings the live session back.
	if _, err := fg.Open(context.Background(), tk, "sid-1", show); err != nil || shown != 2 || len(f.workspaces) != 1 {
		t.Fatalf("reopen: err=%v shown=%d workspaces=%d", err, shown, len(f.workspaces))
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellQuote = %s", got)
	}
}

func TestForgeKeepsEverythingATaskNeedsUnderVigil(t *testing.T) {
	p := Prompt("每天汇总 issue", "/bin/vigil", "/home/me/.vigil", "f-1")
	for _, want := range []string{"/home/me/.vigil/tasks/<任务id>/scripts/", "/home/me/.vigil/tasks/<任务id>/state/", "不要另建规则文件"} {
		if !strings.Contains(p, want) {
			t.Fatalf("forge prompt missing %q", want)
		}
	}
}

func TestStartShowsTheForgeInHerdrBeforeClaudeBoots(t *testing.T) {
	f := &fakeHerdr{}
	shown := 0
	name, err := (Forge{H: f, Sleep: func(time.Duration) {}}).Start(context.Background(), "每天汇总 issue", func() { shown++ })
	if err != nil || !strings.HasPrefix(name, "forge-") {
		t.Fatalf("start = %q, %v", name, err)
	}
	if len(f.focused) != 1 || f.focused[0] != "w9" || f.focusedAtStart != 1 || shown != 1 {
		t.Fatalf("forge not shown before boot: focused=%v focusedAtStart=%d shown=%d", f.focused, f.focusedAtStart, shown)
	}
}

func TestShowBringsALiveForgeBack(t *testing.T) {
	f := &fakeHerdr{agents: map[string]bool{"forge-a1": true}}
	fg := Forge{H: f}
	if err := fg.Show(context.Background(), "forge-a1"); err != nil || len(f.focused) != 1 || f.focused[0] != "w9" {
		t.Fatalf("show: err=%v focused=%v", err, f.focused)
	}
	if err := fg.Show(context.Background(), "v-slack"); err == nil {
		t.Fatal("showed a session Vigil does not manage")
	}
	if err := fg.Show(context.Background(), "forge-gone"); err == nil {
		t.Fatal("showed a forge that is gone")
	}
}
