// Package exec runs one turn of an agent without a terminal (claude -p, stream-json output).
package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"vigil/internal/proc"
	"vigil/internal/task"
)

// Request is one headless turn.
type Request struct {
	Task          task.Task
	AllowTools    []string // one-shot --allowedTools rules (the owner approved previously denied operations)
	TaskDir       string   // the task's own files; always readable whatever the permission mode
	Prompt        string
	ResumeSession string // continue this session id ("" = new session)
	LogPath       string // raw stream-json is appended here
}

// Outcome is what a headless turn produced.
type Outcome struct {
	Denied    []string // allow rules for operations the permission rules refused; allowing them lets a re-run through
	Refused   []string // "tool：reason" for operations the auto mode classifier refused; no allow rule gets past it
	SessionID string
	FinalText string
	CostUSD   float64
	IsError   bool
	TimedOut  bool
	Err       error // process failure (non-zero exit, could not start)
}

// Headless runs `claude -p`. Bin is the claude executable.
type Headless struct {
	Bin string
}

// Args builds the claude command line for a task.
func Args(t task.Task, resume, taskDir string, allow ...string) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", t.Agent.PermissionMode, "--permission-prompts", "none"}
	if taskDir != "" {
		args = append(args, "--add-dir", taskDir)
	}
	if t.Agent.Model != "" {
		args = append(args, "--model", t.Agent.Model)
	}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	if rules := append(append([]string(nil), t.Agent.AllowedTools...), allow...); len(rules) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, rules...)
	}
	if len(t.Agent.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, t.Agent.DisallowedTools...)
	}
	return append(args, t.Agent.Args...)
}

// ExpandHome turns a leading ~ into the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Run executes one turn, honouring ctx and the task's timeout.
func (h Headless) Run(ctx context.Context, req Request) Outcome {
	ctx, cancel := context.WithTimeout(ctx, req.Task.Run.Timeout.Std())
	defer cancel()

	cmd := osexec.CommandContext(ctx, h.Bin, Args(req.Task, req.ResumeSession, req.TaskDir, req.AllowTools...)...)
	if req.Task.Agent.Cwd != "" {
		cmd.Dir = ExpandHome(req.Task.Agent.Cwd)
	}
	cmd.Env = append(os.Environ(), "VIGIL_TASK="+req.Task.ID)
	if home := os.Getenv("VIGIL_HOME"); home != "" {
		cmd.Env = append(cmd.Env, "VIGIL_HOME="+home)
	}
	cmd.Stdin = strings.NewReader(req.Prompt)
	proc.Prepare(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{Err: fmt.Errorf("stdout pipe: %w", err)}
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8192}
	logf, err := openLog(req.LogPath)
	if err != nil {
		return Outcome{Err: err}
	}
	defer logf.Close()

	if err := cmd.Start(); err != nil {
		return Outcome{Err: fmt.Errorf("start %s: %w", h.Bin, err)}
	}
	stop := proc.Guard(ctx, cmd, stdout)
	defer stop()
	out := ParseStream(io.TeeReader(stdout, logf))
	waitErr := cmd.Wait()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.TimedOut = true
	}
	if waitErr != nil && !out.TimedOut {
		why := strings.TrimSpace(stderr.String())
		if why == "" && out.IsError {
			why = clipText(out.FinalText, 300) // an API error is told only in the result event
		}
		out.Err = fmt.Errorf("claude exited: %w: %s", waitErr, why)
	}
	return out
}

func openLog(path string) (io.WriteCloser, error) {
	if path == "" {
		return nopCloser{io.Discard}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	return f, nil
}

type streamEvent struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	CostUSD   float64 `json:"total_cost_usd"`
	Denials   []struct {
		ToolName  string          `json:"tool_name"`
		ToolUseID string          `json:"tool_use_id"`
		ToolInput json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
	Message struct {
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

// ParseStream reads claude stream-json and extracts the session id, final reply and cost.
// When no result event arrives (killed mid-turn), the last assistant text is used.
func ParseStream(r io.Reader) Outcome {
	var out Outcome
	var lastAssistant string
	sawResult := false
	refusals := map[string]string{} // tool_use_id → the classifier's reason
	_ = proc.Lines(r, func(line []byte) {
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if ev.SessionID != "" {
			out.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "assistant":
			if text := assistantText(ev); text != "" {
				lastAssistant = text
			}
		case "user":
			noteRefusals(ev, refusals)
		case "result":
			sawResult = true
			out.FinalText = ev.Result
			out.IsError = ev.IsError
			out.CostUSD = ev.CostUSD
			out.Denied, out.Refused = splitDenials(ev, refusals)
		}
	})
	if !sawResult || out.FinalText == "" {
		out.FinalText = lastAssistant
	}
	return out
}

// classifierRefusal is how Claude Code words a refusal by the auto mode classifier, as opposed to a
// permission rule. The classifier judges the action itself, so no --allowedTools rule gets it through.
const classifierRefusal = "denied by the Claude Code auto mode classifier"

var classifierReason = regexp.MustCompile(`Reason: (\[[^\]]+\])`)

// noteRefusals records the tool calls in a user event that the auto mode classifier refused.
func noteRefusals(ev streamEvent, refusals map[string]string) {
	for _, c := range ev.Message.Content {
		if c.Type != "tool_result" || !c.IsError || c.ToolUseID == "" {
			continue
		}
		text := resultText(c.Content)
		if !strings.Contains(text, classifierRefusal) {
			continue
		}
		reason := "判定为危险操作"
		if m := classifierReason.FindStringSubmatch(text); m != nil {
			reason = m[1]
		}
		refusals[c.ToolUseID] = reason
	}
}

// resultText reads a tool_result's content, which is either a string or a list of text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

// splitDenials sorts a result's permission denials into rules a re-run could allow and classifier refusals.
func splitDenials(ev streamEvent, refusals map[string]string) (denied, refused []string) {
	for _, d := range ev.Denials {
		if reason, ok := refusals[d.ToolUseID]; ok {
			if note := d.ToolName + "：" + reason; !slices.Contains(refused, note) {
				refused = append(refused, note)
			}
			continue
		}
		if rule := AllowRule(d.ToolName, d.ToolInput); rule != "" && !slices.Contains(denied, rule) {
			denied = append(denied, rule)
		}
	}
	return denied, refused
}

// AllowRule turns a refused tool call into the narrowest --allowedTools rule that would permit it:
// Bash by its command name ("Bash(touch:*)"), every other tool by name.
func AllowRule(tool string, input json.RawMessage) string {
	if tool != "Bash" {
		return tool
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	name := ""
	for _, f := range strings.Fields(in.Command) {
		f = strings.TrimRight(f, ";&|")
		if f == "" || isAssignment(f) {
			continue
		}
		name = f
		break
	}
	if name == "" || strings.ContainsAny(name, "()*:,=$`\"'") {
		return ""
	}
	return "Bash(" + name + ":*)"
}

// isAssignment reports whether a shell word is a leading VAR=value assignment.
func isAssignment(word string) bool {
	i := strings.IndexByte(word, '=')
	if i <= 0 {
		return false
	}
	for _, r := range word[:i] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func assistantText(ev streamEvent) string {
	var parts []string
	for _, c := range ev.Message.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	chunk := p
	if len(chunk) > l.n {
		chunk = chunk[:l.n]
	}
	l.n -= len(chunk)
	if _, err := l.w.Write(chunk); err != nil {
		return 0, err
	}
	return len(p), nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func clipText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}
