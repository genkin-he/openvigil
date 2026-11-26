package resident

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// A resident agent is one interactive session: a call that hits one of the owner's ask rules raises
// "Do you want to proceed?" and stops the whole session until the owner answers, while every later round
// detours into a fresh one. The task briefs already say to hand such calls to a background job (vigil
// spawn), where a blocked call becomes an allow-once / allow-always / deny decision instead of a dialog,
// but a brief is only read, not enforced. The guard is a PreToolUse hook given to resident sessions only:
// it turns such a call into an immediate refusal that says where to send it. It never allows anything
// the rules would not; headless jobs run without it, so their denials still reach the owner as decisions.

// AskRules are the owner's ask rules from a Claude Code settings file.
func AskRules(settings []byte) ([]string, error) {
	var s struct {
		Permissions struct {
			Ask []string `json:"ask"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	return s.Permissions.Ask, nil
}

// Asks reports the first rule that would raise a dialog for this call. Rules follow Claude Code's
// syntax: a bare tool name, Bash(prefix:*) for a command prefix, or Bash(pattern) where * matches anything.
func Asks(rules []string, tool, command string) (string, bool) {
	for _, r := range rules {
		name, spec, hasSpec := strings.Cut(r, "(")
		if name != tool {
			continue
		}
		if !hasSpec {
			return r, true
		}
		if tool == "Bash" && matchCommand(strings.TrimSuffix(spec, ")"), command) {
			return r, true
		}
	}
	return "", false
}

func matchCommand(spec, command string) bool {
	if prefix, ok := strings.CutSuffix(spec, ":*"); ok {
		rest, found := strings.CutPrefix(command, prefix)
		return found && (rest == "" || strings.ContainsAny(rest[:1], " \t\n;&|"))
	}
	parts := strings.Split(spec, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?s)^` + strings.Join(parts, ".*") + `$`).MatchString(command)
}

// GuardVerdict is the hook's answer to one PreToolUse payload: a deny with where to send the work, or
// nothing when the call raises no dialog.
func GuardVerdict(payload []byte, rules []string, taskID string) ([]byte, bool, error) {
	var in struct {
		Tool  string `json:"tool_name"`
		Input struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, false, fmt.Errorf("read hook payload: %w", err)
	}
	rule, hit := Asks(rules, in.Tool, in.Input.Command)
	if !hit {
		return nil, false, nil
	}
	reason := fmt.Sprintf("这条命中主人的确认规则 %s：在常驻会话里跑会弹确认框，把整个值守卡住。"+
		"改用 vigil spawn %s --title … --input - 交给后台子任务（被拦时 Vigil 会转成待办请主人决定）；"+
		"在 vigil 命令的文字里提到这类命令时只用文字描述，不要原样写出参数。", rule, taskID)
	out, err := json.Marshal(map[string]map[string]string{"hookSpecificOutput": {
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
	return out, true, err
}

// GuardArgs are the extra arguments that start a resident claude session with the guard; vigilBin is the
// shell command that runs this binary (already quoted).
func GuardArgs(vigilBin, taskID string) []string {
	if vigilBin == "" {
		return nil
	}
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "*",
		"hooks":   []any{map[string]string{"type": "command", "command": vigilBin + " guard --task " + shellQuote(taskID)}},
	}}}})
	return []string{"--settings", string(settings)}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
