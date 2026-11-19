package main

import (
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"vigil/internal/runner"
)

const (
	jobWait   = 2*time.Hour + 10*time.Minute // longest an install waits for background jobs (their timeout is 2h)
	drainWait = 10 * time.Minute             // longest it then waits for the rounds in progress
	drainPoll = 3 * time.Second
)

type drainState struct {
	Draining bool             `json:"draining"`
	Running  []runner.LiveRun `json:"running"`
	Pending  int              `json:"pending"`
}

// drainBeforeRestart keeps a restart from cutting work off halfway. First it holds new background jobs
// and waits for the running ones (they are long, and a cut job starts over), while rounds that answer
// people keep running. Then it holds everything and waits for the rounds in progress. Either wait gives
// up after its limit (cut-off runs are re-run on start). A daemon too old to drain, or not running, is
// restarted at once.
func drainBeforeRestart() {
	c, err := newClient()
	if err != nil {
		return
	}
	c.http.Timeout = 5 * time.Second
	// Ctrl-C while waiting: let the daemon carry on as if nothing happened.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	phases := []struct {
		body  map[string]bool
		wait  time.Duration
		which func(runner.LiveRun) bool
		what  string
	}{
		{map[string]bool{"on": true, "jobs": true}, jobWait, func(r runner.LiveRun) bool { return r.Job != "" }, "后台子任务"},
		{map[string]bool{"on": true}, drainWait, func(runner.LiveRun) bool { return true }, "轮"},
	}
	var st drainState
	for _, p := range phases {
		if err := c.call("POST", "/api/drain", p.body, &st); err != nil {
			fmt.Printf("没法让守护进程先排空（%v），直接重启\n", err)
			return
		}
		if !waitRuns(c, sig, &st, p.wait, p.which, p.what) {
			return
		}
	}
	if st.Pending > 0 {
		fmt.Printf("没有进行中的轮次了；排队的 %d 个请求重启后接着跑\n", st.Pending)
	}
}

// waitRuns polls until no run picked by which is going. It returns false when the restart should
// happen right away (a poll failed or the wait ran out).
func waitRuns(c *client, sig chan os.Signal, st *drainState, wait time.Duration, which func(runner.LiveRun) bool, what string) bool {
	deadline := time.Now().Add(wait)
	last := ""
	for {
		going := slices.DeleteFunc(slices.Clone(st.Running), func(r runner.LiveRun) bool { return !which(r) })
		if len(going) == 0 {
			return true
		}
		if line := describeRunning(going); line != last {
			fmt.Printf("等待进行中的 %d 个%s结束后再重启：%s\n", len(going), what, line)
			last = line
		}
		if time.Now().After(deadline) {
			fmt.Printf("等了 %s 还没结束，直接重启（被打断的会在启动后带着提醒重跑）\n", wait)
			return false
		}
		select {
		case <-sig:
			_ = c.call("POST", "/api/drain", map[string]bool{"on": false}, nil)
			fmt.Println("已取消安装，守护进程照常运行")
			os.Exit(130)
		case <-time.After(drainPoll):
		}
		if err := c.call("GET", "/api/drain", nil, st); err != nil {
			fmt.Printf("查询运行状态失败（%v），直接重启\n", err)
			return false
		}
	}
}

func describeRunning(runs []runner.LiveRun) string {
	parts := make([]string, 0, len(runs))
	for _, r := range runs {
		name := r.TaskID
		if r.Title != "" {
			name += " 后台：" + r.Title
		}
		parts = append(parts, fmt.Sprintf("%s（已跑 %s）", name, time.Since(r.StartedAt).Round(time.Second)))
	}
	return strings.Join(parts, "，")
}
