package exec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"vigil/internal/task"
)

const sampleStream = `{"type":"system","subtype":"init","session_id":"s-1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"正在处理"}]},"session_id":"s-1"}
not json
{"type":"result","subtype":"success","result":"完成\n` + "```json\\n{\\\"status\\\":\\\"ok\\\",\\\"summary\\\":\\\"好\\\"}\\n```" + `","is_error":false,"total_cost_usd":0.12,"session_id":"s-1"}
`

func TestParseStream(t *testing.T) {
	out := ParseStream(strings.NewReader(sampleStream))
	if out.SessionID != "s-1" || out.CostUSD != 0.12 || !strings.Contains(out.FinalText, `"status":"ok"`) {
		t.Fatalf("ParseStream = %+v", out)
	}
}

func TestParseStreamWithoutResultUsesLastAssistant(t *testing.T) {
	out := ParseStream(strings.NewReader(`{"type":"assistant","message":{"content":[{"type":"text","text":"半途"}]},"session_id":"s-2"}` + "\n"))
	if out.FinalText != "半途" || out.SessionID != "s-2" {
		t.Fatalf("ParseStream = %+v", out)
	}
}

func TestArgs(t *testing.T) {
	tk := task.WithDefaults(task.Task{ID: "a", Trigger: task.Trigger{Kind: task.TriggerManual},
		Agent: task.Agent{Model: "claude-opus-5-5", DisallowedTools: []string{"Bash(git push:*)"}, Args: []string{"--add-dir", "/x"}}})
	got := Args(tk, "s-9", "/v/tasks/a")
	for _, want := range []string{"-p", "stream-json", "auto", "none", "claude-opus-5-5", "s-9", "Bash(git push:*)", "/x", "--add-dir", "/v/tasks/a"} {
		if !slices.Contains(got, want) {
			t.Fatalf("Args missing %q: %v", want, got)
		}
	}
}

func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testTask(timeout time.Duration) task.Task {
	tk := task.WithDefaults(task.Task{ID: "a", Trigger: task.Trigger{Kind: task.TriggerManual}})
	tk.Run.Timeout = task.Duration(timeout)
	return tk
}

func TestRunReadsPromptFromStdinAndLogs(t *testing.T) {
	bin := fakeClaude(t, `read -r line; printf '{"type":"result","result":"%s","session_id":"s-3"}\n' "$line"`)
	logPath := filepath.Join(t.TempDir(), "logs", "r.jsonl")
	out := Headless{Bin: bin}.Run(context.Background(), Request{Task: testTask(time.Minute), Prompt: "你好\n", LogPath: logPath})
	if out.Err != nil || out.FinalText != "你好" || out.SessionID != "s-3" {
		t.Fatalf("Run = %+v", out)
	}
	if data, _ := os.ReadFile(logPath); !strings.Contains(string(data), "s-3") {
		t.Fatalf("log not written: %q", data)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	bin := fakeClaude(t, `sleep 30 & wait`)
	tk := testTask(time.Minute)
	tk.Run.Timeout = task.Duration(300 * time.Millisecond)
	start := time.Now()
	out := Headless{Bin: bin}.Run(context.Background(), Request{Task: tk, Prompt: "x"})
	if !out.TimedOut {
		t.Fatalf("expected timeout, got %+v", out)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("kill took too long: %v", time.Since(start))
	}
}

func TestRunReportsExitError(t *testing.T) {
	bin := fakeClaude(t, `echo boom >&2; exit 3`)
	out := Headless{Bin: bin}.Run(context.Background(), Request{Task: testTask(time.Minute), Prompt: "x"})
	if out.Err == nil || !strings.Contains(out.Err.Error(), "boom") {
		t.Fatalf("Run = %+v", out)
	}
}

func TestParseStreamDenials(t *testing.T) {
	src := `{"type":"result","result":"被拦了","permission_denials":[{"tool_name":"Bash","tool_input":{"command":"touch a && ls"}},{"tool_name":"Bash","tool_input":{"command":"touch b"}},{"tool_name":"Write","tool_input":{"file_path":"/x"}}]}` + "\n"
	out := ParseStream(strings.NewReader(src))
	if !slices.Equal(out.Denied, []string{"Bash(touch:*)", "Write"}) {
		t.Fatalf("Denied = %v", out.Denied)
	}
	if !slices.Contains(Args(testTask(time.Minute), "", "", "Bash(touch:*)"), "Bash(touch:*)") {
		t.Fatal("allow rules not passed to claude")
	}
}

// The rule alone ("Bash(deploycli:*)") covers a read and a production deploy alike: the owner decides on the
// command itself, so each refused call keeps what it was for and the full line (11-25 18:28: a card asked to
// allow Bash(deploycli:*) and the owner had to ask what was actually blocked).
func TestParseStreamKeepsWhatEachRefusedCallWas(t *testing.T) {
	src := `{"type":"result","result":"x","permission_denials":[` +
		`{"tool_name":"Bash","tool_input":{"command":"deploycli status --env staging app-a","description":"看测试环境的发布状态"}},` +
		`{"tool_name":"Bash","tool_input":{"command":"deploycli rollout --env staging app-a"}},` +
		`{"tool_name":"Write","tool_input":{"file_path":"/repo/a.go","content":"x"}}]}` + "\n"
	out := ParseStream(strings.NewReader(src))
	want := []Blocked{
		{Rule: "Bash(deploycli:*)", What: "看测试环境的发布状态：deploycli status --env staging app-a"},
		{Rule: "Bash(deploycli:*)", What: "deploycli rollout --env staging app-a"},
		{Rule: "Write", What: "Write /repo/a.go"},
	}
	if !slices.Equal(out.Blocked, want) {
		t.Fatalf("Blocked = %+v", out.Blocked)
	}
	if !slices.Equal(out.Denied, []string{"Bash(deploycli:*)", "Write"}) {
		t.Fatalf("Denied = %v", out.Denied)
	}
}

func TestBlockedCommandsAreCutToOneShortLine(t *testing.T) {
	long := strings.Repeat("a", 300)
	src := `{"type":"result","result":"x","permission_denials":[{"tool_name":"Bash","tool_input":{"command":"deploycli run \\\n  --x ` + long + `"}}]}` + "\n"
	got := ParseStream(strings.NewReader(src)).Blocked[0].What
	if strings.Contains(got, "\n") || len([]rune(got)) > blockedMax+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("What = %q", got)
	}
}

func TestParseStreamLeavesClassifierRefusalsOutOfDenied(t *testing.T) {
	// The auto mode classifier refuses by judging the action itself; an allow rule does not change its mind,
	// so offering "allow and re-run" for it only loops. Only rule-based refusals become Denied.
	src := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"Permission for this action was denied by the Claude Code auto mode classifier. Reason: [Self-Modification]."}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"Claude requested permissions to use Bash, but you haven't granted it yet."}]}]}}
{"type":"result","result":"x","permission_denials":[{"tool_name":"Edit","tool_use_id":"t1","tool_input":{"file_path":"/x"}},{"tool_name":"Bash","tool_use_id":"t2","tool_input":{"command":"touch a"}}]}
`
	out := ParseStream(strings.NewReader(src))
	if !slices.Equal(out.Denied, []string{"Bash(touch:*)"}) {
		t.Fatalf("Denied = %v", out.Denied)
	}
	if !slices.Equal(out.Refused, []string{"Edit：[Self-Modification]"}) {
		t.Fatalf("Refused = %v", out.Refused)
	}
}

func TestAllowRuleSkipsAssignments(t *testing.T) {
	cases := map[string]string{
		`D=/tmp/x; cat "$D/brief.md"`: "Bash(cat:*)",
		`FOO=1 BAR=2 make test`:       "Bash(make:*)",
		`touch done.txt && ls`:        "Bash(touch:*)",
		`$(echo rm) -rf /`:            "",
	}
	for cmd, want := range cases {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		if got := AllowRule("Bash", in); got != want {
			t.Fatalf("AllowRule(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestPermanentAndOneShotAllowRulesCombine(t *testing.T) {
	tk := testTask(time.Minute)
	tk.Agent.AllowedTools = []string{"Bash(glab:*)"}
	got := Args(tk, "", "", "Bash(touch:*)")
	i := slices.Index(got, "--allowedTools")
	if i < 0 || got[i+1] != "Bash(glab:*)" || got[i+2] != "Bash(touch:*)" {
		t.Fatalf("Args = %v", got)
	}
}

// An API error leaves stderr empty and says why only in the result event; that is the reason to show.
func TestRunReportsTheAPIErrorWhenStderrIsEmpty(t *testing.T) {
	bin := fakeClaude(t, `printf '{"type":"result","is_error":true,"result":"API Error: Request blocked before it reached Anthropic API (HTTP 403)"}\n'; exit 1`)
	out := Headless{Bin: bin}.Run(context.Background(), Request{Task: testTask(time.Minute), Prompt: "x"})
	if out.Err == nil || !strings.Contains(out.Err.Error(), "HTTP 403") {
		t.Fatalf("Run = %+v", out.Err)
	}
}
