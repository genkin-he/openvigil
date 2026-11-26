package resident

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vigil/internal/herdr"
)

const ownerSettings = `{"permissions":{"allow":["Bash(git:*)"],"ask":[
  "Bash(*--deployment prod*)","Bash(*--env prod*)","Bash(shipit release:*)","mcp__metrics__create_dashboard"]}}`

func TestAskRulesAreReadFromTheOwnersSettings(t *testing.T) {
	rules, err := AskRules([]byte(ownerSettings))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 || rules[3] != "mcp__metrics__create_dashboard" {
		t.Fatalf("rules = %q", rules)
	}
	if rules, err := AskRules([]byte(`{}`)); err != nil || len(rules) != 0 {
		t.Fatalf("no ask rules: %q %v", rules, err)
	}
}

func TestACallThatWouldRaiseADialogIsFound(t *testing.T) {
	rules, _ := AskRules([]byte(ownerSettings))
	cases := []struct {
		tool, cmd string
		hit       string
	}{
		{"Bash", "cd /tmp && deployctl apply --deployment prod --yes", "Bash(*--deployment prod*)"},
		{"Bash", "python3 -c '\nimport os\n' --env prod", "Bash(*--env prod*)"}, // spans lines
		{"Bash", "shipit release 1 --start", "Bash(shipit release:*)"},
		{"Bash", "shipit release", "Bash(shipit release:*)"},
		{"Bash", "dbq query prod/app --sql 'select 1'", ""},
		{"Bash", "shipit release-us list", ""}, // a prefix rule needs a word boundary
		{"mcp__metrics__create_dashboard", "", "mcp__metrics__create_dashboard"},
		{"mcp__metrics__get_dashboard", "", ""},
	}
	for _, c := range cases {
		got, ok := Asks(rules, c.tool, c.cmd)
		if ok != (c.hit != "") || got != c.hit {
			t.Errorf("%s %q → %q %v, want %q", c.tool, c.cmd, got, ok, c.hit)
		}
	}
}

func TestTheGuardDeniesWithWhereToSendTheWork(t *testing.T) {
	rules, _ := AskRules([]byte(ownerSettings))
	in := `{"tool_name":"Bash","tool_input":{"command":"deployctl x --deployment prod"}}`
	out, denied, err := GuardVerdict([]byte(in), rules, "chat-watch")
	if err != nil || !denied {
		t.Fatalf("denied=%v err=%v", denied, err)
	}
	var raw map[string]map[string]string
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	h := raw["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" ||
		!strings.Contains(h["permissionDecisionReason"], "vigil spawn chat-watch") ||
		!strings.Contains(h["permissionDecisionReason"], "Bash(*--deployment prod*)") {
		t.Fatalf("verdict = %s", out)
	}
	if out, denied, err := GuardVerdict([]byte(`{"tool_name":"Bash","tool_input":{"command":"git status"}}`), rules, "chat-watch"); err != nil || denied || len(out) != 0 {
		t.Fatalf("a plain call was touched: %s %v %v", out, denied, err)
	}
}

func TestTheResidentAgentStartsWithTheGuard(t *testing.T) {
	args := GuardArgs("'/usr/local/bin/vigil'", "chat-watch")
	if len(args) != 2 || args[0] != "--settings" {
		t.Fatalf("args = %q", args)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(args[1]), &s); err != nil {
		t.Fatal(err)
	}
	pre := s.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Hooks[0].Command != "'/usr/local/bin/vigil' guard --task 'chat-watch'" {
		t.Fatalf("settings = %s", args[1])
	}
	if GuardArgs("", "chat-watch") != nil {
		t.Fatal("a guard without a vigil binary")
	}
}

func TestEnsureStartsAClaudeAgentWithTheGuard(t *testing.T) {
	f := &fakeHerdr{statuses: []string{herdr.StatusIdle}}
	e := exe(f)
	e.Guard = "'/bin/vigil'"
	tk := residentTask()
	tk.Agent.Args = []string{"--model", "opus"}
	if _, err := e.Ensure(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	if len(f.startArgs) != 4 || f.startArgs[0] != "--model" || f.startArgs[2] != "--settings" ||
		!strings.Contains(f.startArgs[3], "guard --task") {
		t.Fatalf("args = %q", f.startArgs)
	}
	if len(tk.Agent.Args) != 2 {
		t.Fatalf("the task's own args were changed: %q", tk.Agent.Args)
	}
}
