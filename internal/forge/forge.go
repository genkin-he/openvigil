// Package forge opens interactive agent sessions in the owner's herdr: forging new tasks and taking over a task's run.
package forge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"vigil/internal/exec"
	"vigil/internal/herdr"
	"vigil/internal/resident"
	"vigil/internal/task"
)

const (
	forgePrefix    = "forge-"
	takeoverPrefix = "vo-"
	screenLines    = 200
	readyWait      = 90 * time.Second
)

// Herdr is the herdr surface used here.
type Herdr interface {
	AgentGet(ctx context.Context, name string) (herdr.Agent, error)
	AgentList(ctx context.Context) ([]herdr.Agent, error)
	CreateWorkspace(ctx context.Context, cwd, label string) (string, error)
	StartAgent(ctx context.Context, name, kind, pane string, args []string) error
	Prompt(ctx context.Context, name, text string) error
	Read(ctx context.Context, name string, lines int) (string, error)
	SendKeys(ctx context.Context, name string, keys ...string) error
	WorkspaceList(ctx context.Context) ([]herdr.Workspace, error)
	FocusWorkspace(ctx context.Context, id string) error
	CloseWorkspace(ctx context.Context, id string) error
	PaneRun(ctx context.Context, pane, command string) error
}

// Forge manages interactive sessions in the owner's herdr session.
type Forge struct {
	H               Herdr
	VigilBin        string
	Root            string              // Vigil root, used as the forge working directory
	HerdrBin        string              // herdr executable, for attaching to resident agents
	ResidentSession string              // herdr session that hosts resident agents
	Sleep           func(time.Duration) // waits between prompt checks; nil is time.Sleep
}

// Session is a live forge or takeover agent.
type Session struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Title  string `json:"title"`
}

// TakeoverName is the agent name used when the owner takes over a task.
func TakeoverName(id string) string { return takeoverPrefix + id }

// Held reports whether the owner currently has the task open (scheduled runs should wait).
func (f Forge) Held(ctx context.Context, id string) bool {
	_, err := f.H.AgentGet(ctx, TakeoverName(id))
	return err == nil
}

// Start opens a forge session for idea in the owner's herdr and returns its agent name. The owner talks
// to the forger there (pictures, menus and all), so its workspace is focused and shown before Claude boots.
func (f Forge) Start(ctx context.Context, idea string, shown func()) (string, error) {
	idea = strings.TrimSpace(idea)
	if idea == "" {
		return "", errors.New("idea is empty")
	}
	name := forgePrefix + randomSuffix()
	pane, err := f.H.CreateWorkspace(ctx, f.Root, "Vigil 锻造 · "+short(idea, 16))
	if err != nil {
		return "", fmt.Errorf("create forge workspace: %w", err)
	}
	if err := f.H.FocusWorkspace(ctx, workspaceOf(pane)); err != nil {
		return "", fmt.Errorf("focus forge workspace: %w", err)
	}
	shown()
	if err := f.start(ctx, name, pane, nil); err != nil {
		return "", err
	}
	if err := resident.Submit(ctx, f.H, name, Prompt(idea, f.VigilBin, f.Root, name), f.sleep()); err != nil {
		return "", fmt.Errorf("send forge prompt: %w", err)
	}
	return name, nil
}

// residentLabel names the attach workspace; the task id keeps it stable when the task is renamed.
func residentLabel(t task.Task) string { return "Vigil 常驻 · " + t.Name + " · " + t.ID }

// OpenResident shows a resident task's live agent in the owner's herdr by attaching to it from a new
// workspace. A previous attach workspace of the same task (it may hold the only viewer slot, or its
// attach may have died with a restart) is closed first, and the new attach takes the viewer slot over.
// The agent keeps running in Vigil's session; closing the workspace just detaches.
func (f Forge) OpenResident(ctx context.Context, t task.Task) error {
	if ws, err := f.H.WorkspaceList(ctx); err == nil {
		for _, w := range ws {
			if strings.HasPrefix(w.Label, "Vigil 常驻 · ") && strings.HasSuffix(w.Label, " · "+t.ID) || w.Label == "Vigil 常驻 · "+t.Name {
				_ = f.H.CloseWorkspace(ctx, w.ID)
			}
		}
	}
	pane, err := f.H.CreateWorkspace(ctx, exec.ExpandHome("~"), residentLabel(t))
	if err != nil {
		return fmt.Errorf("create attach workspace: %w", err)
	}
	bin := f.HerdrBin
	if bin == "" {
		bin = "herdr"
	}
	cmd := fmt.Sprintf("%s --session %s agent attach %s --takeover", shellQuote(bin), shellQuote(f.ResidentSession), task.AgentName(t.ID))
	if err := f.H.PaneRun(ctx, pane, cmd); err != nil {
		return fmt.Errorf("attach to %s: %w", task.AgentName(t.ID), err)
	}
	return f.H.FocusWorkspace(ctx, workspaceOf(pane))
}

// workspaceOf returns the workspace id of a pane id ("w3J:p1" → "w3J").
func workspaceOf(pane string) string {
	if i := strings.Index(pane, ":"); i > 0 {
		return pane[:i]
	}
	return pane
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Open takes over a task: it resumes the task's last headless session in the owner's herdr.
// Claude Code takes seconds to boot, so the new workspace is focused and shown (the owner's terminal
// raised) before that wait, letting the owner watch it start instead of staring at an unchanged page.
func (f Forge) Open(ctx context.Context, t task.Task, sessionID string, shown func()) (string, error) {
	name := TakeoverName(t.ID)
	if a, err := f.H.AgentGet(ctx, name); err == nil {
		if err := f.H.FocusWorkspace(ctx, a.WorkspaceID); err != nil {
			return "", err
		}
		shown()
		return name, nil
	}
	if sessionID == "" {
		return "", errors.New("这个任务还没有可以接管的会话（还没跑过，或上一轮失败丢弃了会话）")
	}
	cwd := exec.ExpandHome(t.Agent.Cwd)
	if cwd == "" {
		cwd = exec.ExpandHome("~")
	}
	pane, err := f.H.CreateWorkspace(ctx, cwd, "Vigil 接管 · "+t.Name)
	if err != nil {
		return "", fmt.Errorf("create takeover workspace: %w", err)
	}
	if err := f.H.FocusWorkspace(ctx, workspaceOf(pane)); err != nil {
		return "", fmt.Errorf("focus takeover workspace: %w", err)
	}
	shown()
	if err := f.start(ctx, name, pane, []string{"--resume", sessionID}); err != nil {
		return "", err
	}
	return name, nil
}

func (f Forge) start(ctx context.Context, name, pane string, args []string) error {
	err := f.H.StartAgent(ctx, name, "claude", pane, args)
	deadline := time.Now().Add(readyWait)
	for herdr.IsCode(err, herdr.CodeAgentNotReady) && time.Now().Before(deadline) {
		screen, rerr := f.H.Read(ctx, name, 60)
		if rerr != nil {
			return rerr
		}
		answered := false
		for _, d := range resident.KnownDialogs {
			if strings.Contains(screen, d.Match) {
				if err := f.H.SendKeys(ctx, name, d.Keys...); err != nil {
					return err
				}
				answered = true
			}
		}
		if !answered {
			return errors.New("agent 启动时停在一个对话框上，去 herdr 里处理一下")
		}
		time.Sleep(3 * time.Second)
		a, gerr := f.H.AgentGet(ctx, name)
		switch {
		case gerr != nil:
			err = gerr
		case a.Status == herdr.StatusBlocked:
			err = &herdr.APIError{Code: herdr.CodeAgentNotReady}
		default:
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

// List returns live forge and takeover sessions.
func (f Forge) List(ctx context.Context) ([]Session, error) {
	agents, err := f.H.AgentList(ctx)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, a := range agents {
		if strings.HasPrefix(a.Name, forgePrefix) || strings.HasPrefix(a.Name, takeoverPrefix) {
			out = append(out, Session{Name: a.Name, Status: a.Status, Title: a.Title})
		}
	}
	return out, nil
}

// Say sends the owner's message to a forge or takeover session.
func (f Forge) Say(ctx context.Context, name, text string) error {
	if !managed(name) {
		return fmt.Errorf("%s is not a Vigil session", name)
	}
	err := f.H.Prompt(ctx, name, text)
	if !herdr.IsCode(err, herdr.CodeAgentBlocked) {
		return err
	}
	return f.choose(ctx, name, text)
}

// choose answers the dialog a session is stopped on (a question with numbered options, a permission
// prompt): text names an option by its number or its words.
func (f Forge) choose(ctx context.Context, name, text string) error {
	screen, err := f.H.Read(ctx, name, screenLines)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	p := resident.ReadPrompt(screen)
	if len(p.Options) == 0 {
		return errors.New("agent 停在一个对话框上，去 herdr 里处理一下")
	}
	key, ok := p.Choose(text)
	if !ok {
		return fmt.Errorf("agent 正停在选择框上，回复选项编号或原文：\n%s", p.Choices())
	}
	if err := f.H.SendKeys(ctx, name, key); err != nil {
		return fmt.Errorf("press %s: %w", key, err)
	}
	return nil
}

func (f Forge) sleep() func(time.Duration) {
	if f.Sleep == nil {
		return time.Sleep
	}
	return f.Sleep
}

// Show focuses a live forge or takeover session's workspace in the owner's herdr.
func (f Forge) Show(ctx context.Context, name string) error {
	if !managed(name) {
		return fmt.Errorf("%s is not a Vigil session", name)
	}
	a, err := f.H.AgentGet(ctx, name)
	if err != nil {
		return fmt.Errorf("锻造会话 %s 已经结束了：%w", name, err)
	}
	return f.H.FocusWorkspace(ctx, a.WorkspaceID)
}

// Screen returns the recent terminal of a forge or takeover session.
func (f Forge) Screen(ctx context.Context, name string) (string, error) {
	if !managed(name) {
		return "", fmt.Errorf("%s is not a Vigil session", name)
	}
	return f.H.Read(ctx, name, screenLines)
}

func managed(name string) bool {
	return strings.HasPrefix(name, forgePrefix) || strings.HasPrefix(name, takeoverPrefix)
}

func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func short(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// Close ends a forge or takeover session by closing the workspace that hosts it.
func (f Forge) Close(ctx context.Context, name string) error {
	if !managed(name) {
		return fmt.Errorf("%s is not a Vigil session", name)
	}
	a, err := f.H.AgentGet(ctx, name)
	if errors.Is(err, herdr.ErrNoAgent) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	return f.H.CloseWorkspace(ctx, a.WorkspaceID)
}
