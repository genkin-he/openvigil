// Package prompt builds the text sent to an agent for one run.
package prompt

import (
	"fmt"
	"strings"

	"vigil/internal/task"
)

const maxInput = 12000

// Context is everything a run prompt needs.
type Context struct {
	Task        task.Task
	Dir         string // task directory
	RunID       string
	RunNumber   int
	Trigger     string
	Input       string   // stream events, the owner's answer, or a manual note
	LastSummary string   // previous run's summary
	Failures    []string // summaries of recent consecutive failures
	VigilBin    string   // absolute path of the vigil binary (resident mode reports through it)
	Job         string   // set for a background job: what the main run handed off
	OwnerDoc    string   // the owner's principles shared by all tasks (path; "" when there are none)
}

// Build renders the run prompt.
func Build(c Context) string {
	var b strings.Builder
	if c.Job != "" {
		fmt.Fprintf(&b, "【Vigil 后台子任务】任务「%s」拆出来的子任务「%s」· 运行编号 %s\n\n", c.Task.Name, c.Job, c.RunID)
		b.WriteString("这是主任务交给你的一件耗时的活，主任务已经继续处理别的事了。只做下面「本轮输入」里交代的这一件事，不处理别的事件、不再拆子任务；做完按 brief 的规则对外回复、写待办、汇报。\n")
	} else {
		fmt.Fprintf(&b, "【Vigil 值守】任务「%s」· 第 %d 轮 · 触发：%s · 运行编号 %s\n\n", c.Task.Name, c.RunNumber, c.Trigger, c.RunID)
	}
	fmt.Fprintf(&b, "你是这个任务的长期值守员，由 Vigil 守护进程按时唤醒。每轮都是独立的一次执行，延续性靠下面这些文件，不靠会话记忆。\n")
	fmt.Fprintf(&b, "1. 先读：%s/brief.md（需求，最高优先级）、%s/runbook.md（做法，可能为空）、%s/memory.md（长期记忆，可能为空）。\n", c.Dir, c.Dir, c.Dir)
	b.WriteString("   memory.md 里以「决策：」开头的是我之前对同类问题的决定，遇到同类问题直接照办，不要再问我。\n")
	if c.OwnerDoc != "" {
		fmt.Fprintf(&b, "   还要读 %s：我的判断原则，所有任务共用，从我自己的纠错、回复和决定里提炼出来；代我判断、措辞时照它办。和本任务 brief 冲突时以 brief 为准。\n", c.OwnerDoc)
	}
	b.WriteString("2. 不需要也不要使用 CronCreate、Monitor、/loop、ScheduleWakeup 这类续命手段，调度和事件转发都由 Vigil 负责。\n")
	b.WriteString("   不要用 osascript display notification 弹 mac 通知（点开是脚本编辑器，没用）：要我知道的事写进本轮结果或待办，Vigil 会通知我；brief 里写了「弹 mac 通知」的，也照此跳过。\n")
	b.WriteString("3. 不可撤销或对外的动作（发消息、合并、发布、删除、付款）只在 brief 明确授权时做；否则回报 needs_you，把要你决定的事和选项写清楚。\n")
	if c.Task.Hands.GUI {
		b.WriteString(handsGuide(c))
	}
	if c.LastSummary != "" {
		fmt.Fprintf(&b, "\n上一轮结果：%s\n", oneLine(c.LastSummary))
	}
	if len(c.Failures) >= 2 {
		b.WriteString("\n⚠ 最近连续失败，先诊断原因再执行。失败摘要：\n")
		for _, f := range c.Failures {
			fmt.Fprintf(&b, "- %s\n", oneLine(f))
		}
	}
	if strings.TrimSpace(c.Input) != "" {
		fmt.Fprintf(&b, "\n本轮输入：\n%s\n", clip(c.Input, maxInput))
	} else {
		b.WriteString("\n本轮输入：无（按 brief 例行执行）\n")
	}
	b.WriteString("\n" + reportSpec(c))
	return b.String()
}

// reportQuoteGuide: the report is itself a shell command, checked by the same permission rules as any other,
// so a command quoted verbatim in it (say one with a production flag) can stop it on a confirmation dialog.
const reportQuoteGuide = "汇报命令本身也要过权限规则：summary、memory 里提到命令时用文字描述（比如「生产环境的 list 命令」），不要原样抄命令行参数，否则可能命中确认规则，让汇报卡在确认框上。\n"

func reportSpec(c Context) string {
	schema := `{"status": "ok | needs_you | fail", "summary": "一两句话：做了什么、结果如何（中文）", "question": "needs_you 时：要我决定什么，一句话", "options": ["A: 推荐做法 — 后果", "B: …"], "recommendation": "A", "memory": ["值得长期记住、下一轮要用的事实（可选）"]}`
	links := "\nsummary、question、options 里提到的工单、MR、需求/缺陷、发布单、Slack 消息、文档等，只要有网页地址，一律写成 [名称](完整 https 链接)，例如 [工单 #331](https://…/issues/331)、[!175](https://…/merge_requests/175)；不要只写编号或路径。拿不到地址的才只写编号。\n"
	tail := links + todoGuide(c) + followGuide(c) + spawnGuide(c) + "\n如果发现 runbook 可以写得更好（更快、更稳），在汇报前执行：" + bin(c) + " amend " + c.Task.ID + " -f <新 runbook 文件> --reason \"原因\"\n"
	if c.Task.Mode == task.ModeResident {
		return "做完后必须执行这条命令汇报（单引号内是 JSON）：\n" +
			bin(c) + " done " + c.RunID + " --json '" + schema + "'\n" + reportQuoteGuide + tail
	}
	return "做完后，最后一条回复必须以一个 ```json 代码块结尾，格式：\n```json\n" + schema + "\n```\n" + tail
}

// spawnGuide tells a main run to hand long work to a background job instead of holding up the events behind it.
func spawnGuide(c Context) string {
	if c.Job != "" {
		return ""
	}
	return "\n耗时长的活不要在本轮里做完：预计要超过 5 分钟的（深入排查、改代码提 MR、跑测试 / 部署、等 CI），用下面的命令交给后台子任务，本轮接着处理别的事，summary 写「已转后台 <返回的编号>」：\n" +
		bin(c) + " spawn " + c.Task.ID + " --title \"<一句话：做什么>\" --input -   （从 stdin 读：要做什么、来源消息 / 链接 / ref、已知的线索、做完怎么回复或写待办，写全，子任务看不到本轮的上下文）\n" +
		"子任务和你用同样的 brief、runbook、权限，独立运行；同一件事别重复转，交出去的事本轮就不要再做了。几分钟内能做完的小事直接做，不要转。\n"
}

// followGuide tells the agent to register what it now waits for, so nothing it asked on the owner's
// behalf is forgotten.
func followGuide(c Context) string {
	return "\n以我的名义问了别人、提了工单 / MR / 审批、把事交给别的团队之后，如果要等对方回复或办完、而且等不到就会耽误事，登记一条跟进，到期时 Vigil 会把它交回本任务（回了就结，没回就追问一次，再没回就交给我）：\n" +
		bin(c) + " follow add --task " + c.Task.ID + " --title \"<在等什么，一句话>\" --who \"<等谁>\" --ask \"<问了什么 / 提了什么，在哪>\" --link \"<去哪看回复：网页或 slack:// 深链>\" --ref \"<唯一键，如 slack:<channel>:<thread_ts>、工单号>\" --due <多久后看：4h / 1d / 3d>\n" +
		"对方已经回了、事情办完了，或者不用再等了：" + bin(c) + " follow done <fu-id> --outcome \"<结果>\"；用 " + bin(c) + " follow list 看在等的事。纯告知、不需要对方回的不用登记。\n"
}

// todoGuide tells the agent how to hand the owner a decision about one concrete matter.
func todoGuide(c Context) string {
	return "\n要我拍板或确认回复的具体事项（某条 Slack 消息、某个缺陷 / MR / 工单），写进 Vigil 待办，我会在 Vigil 控制台回答，回答会作为新一轮输入交回本任务：\n" +
		bin(c) + " todo add --task " + c.Task.ID + " --title \"<人>：<一句话>\" --kind \"<事情的类别>\" --detail \"<频道 / 背景>\" --link \"<网页或 slack:// 深链>\" --ref \"<来源唯一键，如 <channel>:<ts>[:<thread_ts>]>\" --target \"<回复发到哪，如 slack:<channel>:<thread_ts> 或 issue:<链接>>\" --analysis \"<查了什么、结论和依据、要我定什么；多行用 - 从 stdin 读>\" --reply \"<可以直接发出去的建议回复；拿不准或要我拍板的不带>\"\n" +
		"--kind 是稳定的短类别（2～6 个字，同类事每次用同一个词，例如「问进度」「问数据」「要审批」「缺陷确认」「要排期」），Vigil 按类别学习我的处理习惯，所以别每次换说法；先用 " + bin(c) + " todo kinds " + c.Task.ID + " 看已有的类别，能归进去就用已有的。\n" +
		"要我在几个明确做法里选一个的（审批：批这一次 / 不批 / 以后都自动批；要不要做某事；几种方案），加 --choice，每个选项一个：--choice \"<按钮>|<后果>\"，按钮不超过 30 字、2～5 个，后果写清选它会发生什么。我点了哪个，你会收到「我选了「<按钮>」（<后果>）」；我也可能不点按钮、直接写意见。开放问题（要我给数据、写措辞）不要硬凑选项。\n" +
		"同一 --ref 会更新已有待办，不会重复建；写之前可以用 " + bin(c) + " todo list 看已有的。写过待办的事，本轮结果标 ok，summary 写「已建待办 td-xxx」，不要再标 needs_you；needs_you 只留给不能落到具体事项、又要我决定的事（例如任务规则要不要改）。\n"
}

func bin(c Context) string {
	if c.VigilBin != "" {
		return c.VigilBin
	}
	return "vigil"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…（还有 %d 字节被截断）", len(s)-n)
}

func handsGuide(c Context) string {
	return "\n这个任务允许你操作电脑界面（「手」）。Vigil 只在我离开电脑时才运行这类任务，但要假设我随时可能回来：\n" +
		"- 能不碰界面就不碰：优先命令行、AppleScript（osascript）、Shortcuts（shortcuts run）；网页用 browser-act 技能；最后才用 System Events 点按和 screencapture 截屏确认。\n" +
		"- 每做一步界面操作都截屏确认结果，确认不了就停下回报 needs_you，不要盲点。\n" +
		"- 发送、提交、删除、付款、下单这类不可撤销的动作，除非 brief 明确授权，否则停在最后一步，回报 needs_you，写清楚要我确认什么。\n" +
		"- 摸索出能稳定复用的流程，就写成脚本放进 " + c.Dir + "/scripts/，并用 amend 把 runbook 改成直接调用脚本，下次不用再摸索界面。\n"
}
