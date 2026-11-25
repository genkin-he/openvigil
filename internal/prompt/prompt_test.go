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

func TestKnowledgeTopicsAreListedNotInlined(t *testing.T) {
	c := ctx(task.ModeHeadless)
	if strings.Contains(Build(c), "/v/tasks/demo/knowledge/") {
		t.Fatal("no knowledge yet, but the prompt lists some")
	}
	c.Knowledge = []Topic{{Name: "misc", Entries: 3, Bytes: 600}}
	if p := Build(c); !strings.Contains(p, "整份读一遍") {
		t.Fatalf("a small knowledge base is not read whole:\n%s", p)
	}
	c.Knowledge = []Topic{{Name: "user-service", Entries: 23, Bytes: 9000}, {Name: "monitor", Entries: 4, Bytes: 800}}
	p := Build(c)
	if strings.Contains(p, "整份读一遍") {
		t.Fatal("a large knowledge base is read whole")
	}
	for _, want := range []string{"/v/tasks/demo/knowledge/", "user-service.md（23 条）", "monitor.md（4 条）", "[主题]", "先读对应文件"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestChoicesAreToldToRespectTheOwnersPrinciples(t *testing.T) {
	c := ctx(task.ModeHeadless)
	c.OwnerDoc = "/v/owner.md"
	p := Build(c)
	for _, want := range []string{"违背", "/v/owner.md", "完整动作", "转产品定"} {
		if !strings.Contains(p, want) {
			t.Fatalf("choice guidance missing %q", want)
		}
	}
	if p := Build(ctx(task.ModeHeadless)); strings.Contains(p, "违背已定原则") {
		t.Fatal("no principles file, yet the choices are told to keep to it")
	}
}

func TestReplyGuidanceDefersToTheBrief(t *testing.T) {
	p := Build(ctx(task.ModeHeadless))
	if !strings.Contains(p, "什么时候带以 brief 为准") {
		t.Fatal("the shared --reply guidance does not defer to the brief")
	}
	if strings.Contains(p, "--reply \"<可以直接发出去的建议回复；拿不准或要我拍板的不带>\"") {
		t.Fatal("the shared guidance still forbids drafts outright, overruling briefs that ask for them")
	}
}

// A quarter of the to-dos the owner answered by typing despite choices were "go check X first"
// (does the field exist, was it fixed before, what did the PM last say): facts the task could
// have looked up before asking.
func TestDecisionsAreRaisedOnlyAfterTheCheckableFacts(t *testing.T) {
	p := Build(ctx(task.ModeHeadless))
	for _, want := range []string{"建要我拍板的待办之前", "之前有没有做过 / 修过", "最新回复", "问谁", "不要把「先查一下」留给我"} {
		if !strings.Contains(p, want) {
			t.Fatalf("decision guidance missing %q", want)
		}
	}
}

func TestOthersDecidingDoesNotCloseTheOwnersOpenTodo(t *testing.T) {
	p := Build(ctx(task.ModeHeadless))
	for _, want := range []string{"别人先拍板", "和待办里给我的选项或拟稿不完全一样", "不要结掉", "todo note", "不要另开"} {
		if !strings.Contains(p, want) {
			t.Fatalf("guidance on others deciding missing %q", want)
		}
	}
}

// A to-do the owner settled himself (approved on the web, replied in Slack) is closed with --by-owner, so the
// north star counts it as his work instead of something that closed on its own (11-24 td-9f6b24).
func TestClosingAToDoTheOwnerHandledHimselfSaysSo(t *testing.T) {
	p := Build(ctx(task.ModeHeadless))
	for _, want := range []string{"我自己已经处理了", "--by-owner"} {
		if !strings.Contains(p, want) {
			t.Fatalf("guidance on closing what the owner did missing %q", want)
		}
	}
}
