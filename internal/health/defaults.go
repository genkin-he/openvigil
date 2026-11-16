package health

// Defaults are the checks every install gets when config.yaml lists none: the claude CLI the agents run
// on, herdr (new tasks and takeovers run in it), and the Slack CLI when Slack is configured. Add your own (a CLI you log in to, an MCP server) under
// checks: in config.yaml — they replace these defaults.
func Defaults(slack bool) []Check {
	out := []Check{{Name: "claude", Command: "claude --version",
		Hint: "找不到 claude CLI 或它跑不起来：安装 Claude Code 并登录（claude 命令要在 PATH 里），再点复查"},
		{Name: "herdr", Command: "herdr --version",
			Hint: "找不到 herdr：新建任务、接管会话都要用它。在终端跑 curl -fsSL https://herdr.dev/install.sh | sh 安装，再点复查"}}
	if slack {
		out = append(out, Check{Name: "slackctl", Command: "slackctl auth test",
			Fix:  "slackctl auth import-desktop",
			Hint: "Slack 登录失效了：确认 Slack 桌面版已登录，然后在终端跑 slackctl auth import-desktop"})
	}
	return out
}
