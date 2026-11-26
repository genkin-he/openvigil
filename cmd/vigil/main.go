// Command vigil is Vigil's daemon and CLI.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// version is set at build time (make build / make release); "dev" for a plain go build.
var version = "dev"

type command struct {
	usage string
	run   func(args []string) error
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"daemon":    {"vigil daemon                          运行守护进程（launchd 调用）", cmdDaemon},
		"install":   {"vigil install [--force] [--no-selftest]   安装为 launchd 服务并启动（先等进行中的轮次跑完；装完后台自测）", cmdInstall},
		"follow":    {"vigil follow add --task --title --due 1d [--who --ask --link --ref] | list [--all] | done <fu-id> --outcome | snooze <fu-id> --due 1d [--nudged]   等别人回复的跟进", cmdFollow},
		"evolve":    {"vigil evolve signals [--since RFC3339] [--task <id>] | ledger | record --task --target --change --why --expect [--backup] [--todo] [--metric <类别>] [--review-days 7] | verdict <ev-id> --keep|--revert --note | replay --task <id> --doc brief.md|runbook.md|memory.md|owner.md --after-file <f> [--before-file <f>] [--kind <类别>] [--runs a,b]", cmdEvolve},
		"owner":     {"vigil owner [show] | append \"<原则>\" | set -f <文件>   我的判断原则（所有任务每轮都读）", cmdOwner},
		"retro":     {"vigil retro [--days 7]                 一段时间的运行、失败原因、成本、待办处理统计（JSON）", cmdRetro},
		"selftest":  {"vigil selftest [--status]              端到端自测：Slack 监听回复、待办发送、后台子任务", cmdSelfTest},
		"uninstall": {"vigil uninstall                       停止并移除 launchd 服务", cmdUninstall},
		"status":    {"vigil status                          守护进程和任务概况", cmdStatus},
		"ls":        {"vigil ls                              任务列表", cmdList},
		"show":      {"vigil show <id>                       任务详情和最近几轮", cmdShow},
		"logs":      {"vigil logs <id> [-n 20]               最近几轮的结果", cmdLogs},
		"web":       {"vigil web                             在浏览器打开控制台", cmdWeb},
		"new":       {"vigil new \"<一句话需求>\"              在 herdr 里起锻造会话", cmdNew},
		"apply":     {"vigil apply -f task.yaml [--brief f] [--runbook f]   创建或更新任务", cmdApply},
		"run":       {"vigil run <id> [--input 文字] [--wait] [--trial]     立即跑一轮", cmdRun},
		"pause":     {"vigil pause <id>                      暂停", cmdPause},
		"resume":    {"vigil resume <id>                     启用 / 恢复", cmdResume},
		"rm":        {"vigil rm <id>                         删除任务", cmdRemove},
		"open":      {"vigil open <id>                       接管：在 herdr 里打开最近一轮的会话", cmdOpen},
		"answer":    {"vigil answer <id> <回答>               回答 needs_you，作为下一轮输入", cmdAnswer},
		"done":      {"vigil done <run-id> --json '{...}'    常驻任务回报本轮结果", cmdDone},
		"memory":    {"vigil memory <id> append [--topic <主题>] \"...\"  追加记忆：规则进 memory.md，带主题的事实进 knowledge/<主题>.md", cmdMemory},
		"amend":     {"vigil amend <id> -f runbook.md --reason \"...\"   提议改进 runbook", cmdAmend},
		"spawn":     {"vigil spawn <id> --title \"…\" --input -|文字       把耗时的活交给后台子任务（不阻塞主任务）", cmdSpawn},
		"todo":      {"vigil todo add|list|done|note …              待办：agent 提交要我拍板的事，我在控制台回答", cmdTodo},
		"export":    {"vigil export [-o 包.tar.gz]             搬家：跑完进行中的活、停掉服务，把数据打成一个包（登录和 token 不打包）", cmdExport},
		"import":    {"vigil import <包> [--force] [--no-install]   搬家：在新电脑解包、改路径、体检，全部就绪才安装启动", cmdImport},
		"doctor":    {"vigil doctor                          体检：工具、登录、任务用到的仓库和目录是否都在", cmdDoctor},
		"guard":     {"vigil guard --task <id>               常驻会话的 PreToolUse 钩子：会弹确认框的调用当场拒掉，改交后台子任务（Vigil 自动挂上，不用手动跑）", cmdGuard},
		"patch":     {"vigil patch propose --task <id> --title \"…\" --why \"…\" -f change.diff   提议改 Vigil 自己的代码（git diff），主人在控制台确认后自动测试、合并、安装", cmdPatch},
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Vigil —— 不会过期、会自愈的 AI 值守\n\n用法：")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(os.Stderr, "  "+commands[n].usage)
	}
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("vigil " + version)
		return
	}
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "未知命令：%s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err := cmd.run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误："+strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
