// Package herdr drives a herdr session through its CLI (JSON in, JSON out).
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"syscall"
	"time"
)

// Error codes reported by herdr that callers branch on.
const (
	CodeAgentNotReady = "agent_not_ready"
	CodeAgentBlocked  = "agent_blocked"
	CodeServerDown    = "server_not_running"
)

// Agent statuses.
const (
	StatusIdle    = "idle"
	StatusWorking = "working"
	StatusBlocked = "blocked"
	StatusDone    = "done"
	StatusUnknown = "unknown"
)

const callTimeout = 90 * time.Second

// ErrNoAgent means no live agent has the requested name.
var ErrNoAgent = errors.New("agent not found")

// APIError is an error object returned by the herdr server.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// IsCode reports whether err is a herdr API error with the given code.
func IsCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == code
}

// Agent is the part of herdr's agent object Vigil uses.
type Agent struct {
	Name        string `json:"name"`
	Kind        string `json:"agent"`
	Status      string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	Title       string `json:"terminal_title_stripped"`
	Session     *struct {
		Value string `json:"value"`
	} `json:"agent_session"`
}

// SessionID returns the agent's own session id (e.g. Claude's), if herdr knows it.
func (a Agent) SessionID() string {
	if a.Session == nil {
		return ""
	}
	return a.Session.Value
}

// Runner executes herdr with arguments and returns stdout, stderr.
type Runner func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

// Client talks to one herdr session.
type Client struct {
	Bin     string
	Session string
	run     Runner
}

// New returns a client for the named session using the real herdr binary.
func New(bin, session string) *Client {
	c := &Client{Bin: bin, Session: session}
	c.run = c.execRun
	return c
}

// NewWithRunner is for tests.
func NewWithRunner(session string, r Runner) *Client {
	return &Client{Bin: "herdr", Session: session, run: r}
}

func (c *Client) execRun(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := osexec.CommandContext(ctx, c.Bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func (c *Client) base() []string {
	if c.Session == "" || c.Session == "default" {
		return nil
	}
	return []string{"--session", c.Session}
}

func (c *Client) call(ctx context.Context, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	stdout, stderr, err := c.run(ctx, append(c.base(), args...)...)
	if err != nil {
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(bytes.TrimSpace(stderr), &env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, fmt.Errorf("herdr %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(stderr)))
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *APIError       `json:"error"`
	}
	if err := json.Unmarshal(stdout, &env); err != nil {
		return nil, fmt.Errorf("herdr %s: decode: %w", args[0], err)
	}
	if env.Error != nil {
		return nil, env.Error
	}
	return env.Result, nil
}

// Running reports whether the session's server answers.
func (c *Client) Running(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := c.run(ctx, append(c.base(), "status", "server")...)
	return err == nil && strings.Contains(string(out), "status: running")
}

// StartServer launches a detached headless server for the session.
func (c *Client) StartServer() error {
	cmd := osexec.Command(c.Bin, append(c.base(), "server")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open devnull: %w", err)
	}
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start herdr server: %w", err)
	}
	return cmd.Process.Release()
}

// AgentGet returns a live agent by name or ErrNoAgent.
func (c *Client) AgentGet(ctx context.Context, name string) (Agent, error) {
	res, err := c.call(ctx, "agent", "get", name)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && strings.Contains(apiErr.Code, "not_found") {
			return Agent{}, ErrNoAgent
		}
		return Agent{}, err
	}
	var out struct {
		Agent Agent `json:"agent"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return Agent{}, fmt.Errorf("decode agent: %w", err)
	}
	return out.Agent, nil
}

// CreateWorkspace creates a workspace and returns its root pane id.
func (c *Client) CreateWorkspace(ctx context.Context, cwd, label string) (string, error) {
	res, err := c.call(ctx, "workspace", "create", "--cwd", cwd, "--label", label, "--no-focus")
	if err != nil {
		return "", err
	}
	var out struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &out); err != nil || out.RootPane.PaneID == "" {
		return "", fmt.Errorf("decode workspace create: %v", err)
	}
	return out.RootPane.PaneID, nil
}

// CloseWorkspace closes a workspace Vigil created.
func (c *Client) CloseWorkspace(ctx context.Context, id string) error {
	_, err := c.call(ctx, "workspace", "close", id)
	return err
}

// StartAgent starts an agent of kind in pane. A blocked startup returns an APIError with CodeAgentNotReady.
func (c *Client) StartAgent(ctx context.Context, name, kind, pane string, args []string) error {
	cmd := []string{"agent", "start", name, "--kind", kind, "--pane", pane, "--timeout", "60000"}
	if len(args) > 0 {
		cmd = append(append(cmd, "--"), args...)
	}
	_, err := c.call(ctx, cmd...)
	return err
}

// Prompt submits text without waiting for the turn to finish.
func (c *Client) Prompt(ctx context.Context, name, text string) error {
	_, err := c.call(ctx, "agent", "prompt", name, text)
	return err
}

// Read returns recent terminal text of an agent.
func (c *Client) Read(ctx context.Context, name string, lines int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	out, stderr, err := c.run(ctx, append(c.base(), "agent", "read", name, "--source", "recent-unwrapped", "--lines", fmt.Sprint(lines))...)
	if err != nil {
		return "", fmt.Errorf("herdr agent read: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return string(out), nil
}

// SendKeys sends logical keys (esc, enter, down, ctrl+c) to an agent.
func (c *Client) SendKeys(ctx context.Context, name string, keys ...string) error {
	_, err := c.call(ctx, append([]string{"agent", "send-keys", name}, keys...)...)
	return err
}

// AgentList returns every live agent in the session.
func (c *Client) AgentList(ctx context.Context) ([]Agent, error) {
	res, err := c.call(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	var out struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode agent list: %w", err)
	}
	return out.Agents, nil
}

// Workspace is the part of herdr's workspace object Vigil uses.
type Workspace struct {
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
}

// WorkspaceList returns the session's workspaces.
func (c *Client) WorkspaceList(ctx context.Context) ([]Workspace, error) {
	res, err := c.call(ctx, "workspace", "list")
	if err != nil {
		return nil, err
	}
	var out struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode workspace list: %w", err)
	}
	return out.Workspaces, nil
}

// FocusWorkspace brings a workspace to the front in attached clients.
func (c *Client) FocusWorkspace(ctx context.Context, id string) error {
	_, err := c.call(ctx, "workspace", "focus", id)
	return err
}

// PaneRun types a command into a shell pane and presses Enter. herdr prints nothing on success.
func (c *Client) PaneRun(ctx context.Context, pane, command string) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	_, stderr, err := c.run(ctx, append(c.base(), "pane", "run", pane, command)...)
	if err != nil {
		return fmt.Errorf("herdr pane run: %w: %s", err, strings.TrimSpace(string(stderr)))
	}
	return nil
}
