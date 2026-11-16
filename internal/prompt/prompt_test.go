package prompt

import (
	"strings"
	"testing"

	"vigil/internal/task"
)

func ctx(mode string) Context {
	return Context{
		Task:      task.WithDefaults(task.Task{ID: "demo", Name: "演示", Mode: mode, Trigger: task.Trigger{Kind: task.TriggerManual}}),
		Dir:       "/v/tasks/demo",
		RunID:     "r-1",
		RunNumber: 3,
		Trigger:   "cron",
		VigilBin:  "/bin/vigil",
	}
}

func TestHeadlessAsksForJSONBlock(t *testing.T) {
	p := Build(ctx(task.ModeHeadless))
	for _, want := range []string{"第 3 轮", "/v/tasks/demo/brief.md", "```json", "本轮输入：无", "「决策：」", "[名称](完整 https 链接)"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, " done r-1") {
		t.Fatal("headless prompt should not ask for vigil done")
	}
}

func TestResidentAsksForDoneCommand(t *testing.T) {
	p := Build(ctx(task.ModeResident))
	if !strings.Contains(p, "/bin/vigil done r-1 --json") {
		t.Fatalf("resident prompt missing done command:\n%s", p)
	}
	if !strings.Contains(p, "不要原样抄命令行参数") {
		t.Fatal("resident prompt does not warn that the report command is checked by permission rules")
	}
	if strings.Contains(Build(ctx(task.ModeHeadless)), "不要原样抄命令行参数") {
		t.Fatal("headless runs report in their reply, not a command; the warning does not apply")
	}
}

func TestFailuresAndInput(t *testing.T) {
	c := ctx(task.ModeHeadless)
	c.Failures = []string{"超时", "登录失效"}
	c.Input = strings.Repeat("x", maxInput+10)
	p := Build(c)
	if !strings.Contains(p, "先诊断原因") || !strings.Contains(p, "登录失效") {
		t.Fatal("failures not included")
	}
	if !strings.Contains(p, "还有 10 字节被截断") {
		t.Fatal("input not clipped")
	}
}

func TestOwnerPrinciplesAreReadEveryRoundWhenPresent(t *testing.T) {
	c := ctx(task.ModeHeadless)
	if strings.Contains(Build(c), "owner.md") {
		t.Fatal("prompt names a principles file that does not exist")
	}
	c.OwnerDoc = "/v/owner.md"
	p := Build(c)
	if !strings.Contains(p, "/v/owner.md") || !strings.Contains(p, "以 brief 为准") {
		t.Fatalf("prompt does not point at the principles:\n%s", p)
	}
}

func TestAgentsAreToldNotToPostMacNotifications(t *testing.T) {
	got := Build(ctx(task.ModeHeadless))
	if !strings.Contains(got, "display notification") {
		t.Fatal("the prompt does not forbid osascript notifications")
	}
}

func TestAgentsAreToldToOfferFixedChoices(t *testing.T) {
	out := Build(ctx(task.ModeHeadless))
	if !strings.Contains(out, `--choice "<按钮>|<后果>"`) {
		t.Fatalf("todo guide does not explain choices:\n%s", out)
	}
}
