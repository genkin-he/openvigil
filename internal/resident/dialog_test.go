package resident

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vigil/internal/task"
)

// A Claude Code permission dialog under earlier output, as herdr reads it.
const permissionScreen = `  如果发现 runbook 可以写得更好（更快、更稳），在汇报前执行：vigil amend demo -f <新 runbook
  文件> --reason "原因"
⏺ Bash(vigil done r1 --json '{"status": "ok", "summary":
      "已处理…)
  ⎿  Waiting…
──────────────────────────────────────────────────────────────────────
 Bash command
 回报本轮结果
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 │ vigil done r1 --json '{"status": "ok", "summary":
 │ "已处理", "memory": ["不要跑 deploy list --env prod"]}'
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Ask rule Bash(*--env prod*) overrides auto mode for this command.
 /permissions to let auto mode decide
 Do you want to proceed?
 ❯ 1. Yes
   2. No
 Esc to cancel · Tab to amend
`

func TestReadPromptCutsThePermissionDialogOutOfTheScreen(t *testing.T) {
	p := ReadPrompt(permissionScreen)
	if p.Kind != "Bash command" || p.Question != "Do you want to proceed?" {
		t.Fatalf("prompt = %+v", p)
	}
	if strings.Join(p.Options, "|") != "Yes|No" {
		t.Fatalf("options = %q", p.Options)
	}
	wantCmd := "vigil done r1 --json '{\"status\": \"ok\", \"summary\":\n\"已处理\", \"memory\": [\"不要跑 deploy list --env prod\"]}'"
	if p.Command != wantCmd {
		t.Fatalf("command = %q", p.Command)
	}
	if len(p.Notes) != 3 || p.Notes[0] != "回报本轮结果" || !strings.HasPrefix(p.Notes[1], "Ask rule") {
		t.Fatalf("notes = %q", p.Notes)
	}
	if p.Headline() != "Bash command：回报本轮结果" {
		t.Fatalf("headline = %q", p.Headline())
	}
	text := p.Text()
	for _, gone := range []string{"runbook", "Waiting", "╌", "─", "│"} {
		if strings.Contains(text, gone) {
			t.Fatalf("text keeps %q from outside the dialog:\n%s", gone, text)
		}
	}
	if !strings.HasSuffix(text, "Do you want to proceed? Yes / No") {
		t.Fatalf("text = %s", text)
	}
}

func TestReadPromptKeepsTheTailOfAnUnrecognisedScreen(t *testing.T) {
	screen := strings.Repeat("noise\n", 20) + "Allow rm -rf /? (y/n)\n"
	p := ReadPrompt(screen)
	if p.Question != "" || len(p.Notes) != 1 || !strings.HasSuffix(p.Notes[0], "Allow rm -rf /? (y/n)") ||
		strings.Count(p.Notes[0], "\n") != promptFallbackLines-1 {
		t.Fatalf("prompt = %+v", p)
	}
	if p.Text() != p.Notes[0] {
		t.Fatalf("text = %q", p.Text())
	}
}

func TestReadPromptWithoutARuleTakesTheLinesAboveTheQuestion(t *testing.T) {
	p := ReadPrompt("old output\nEdit file\nmain.go\nDo you want to make this edit?\n> 1. Yes\n  2. No\n")
	if p.Question != "Do you want to make this edit?" || len(p.Options) != 2 || !strings.Contains(p.Text(), "main.go") {
		t.Fatalf("prompt = %+v", p)
	}
}

func TestAnswerDialogPressesTheOptionOnlyOnTheSameDialog(t *testing.T) {
	shown := ReadPrompt(permissionScreen).Body()
	f := &fakeHerdr{screen: permissionScreen}
	e := Executor{H: f}
	if err := e.AnswerDialog(context.Background(), task.Task{ID: "demo"}, shown, "No"); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || strings.Join(f.keys[0], ",") != "2" {
		t.Fatalf("keys = %v", f.keys)
	}

	f.keys = nil
	other := strings.Replace(permissionScreen, "deploy list --env prod", "rm -rf /", 1)
	f.screen = other
	if err := e.AnswerDialog(context.Background(), task.Task{ID: "demo"}, shown, "Yes"); !errors.Is(err, ErrDialogChanged) || len(f.keys) != 0 {
		t.Fatalf("pressed on a different command: err=%v keys=%v", err, f.keys)
	}
	f.screen = strings.Replace(permissionScreen, "   2. No", "   2. Yes, and don't ask again\n   3. No", 1)
	if err := e.AnswerDialog(context.Background(), task.Task{ID: "demo"}, shown, "Yes, and don't ask again"); !errors.Is(err, ErrDialogChanged) || len(f.keys) != 0 {
		t.Fatalf("pressed a lasting allow: err=%v keys=%v", err, f.keys)
	}
}
