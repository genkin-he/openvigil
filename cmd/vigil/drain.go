package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vigil/internal/runner"
)

const (
	drainWait = 10 * time.Minute // longest an install waits for runs in progress
	drainPoll = 3 * time.Second
)

type drainState struct {
	Draining bool             `json:"draining"`
	Running  []runner.LiveRun `json:"running"`
	Pending  int              `json:"pending"`
}

// drainBeforeRestart asks the running daemon to start nothing new and waits for the runs in progress,
// so a restart does not cut a round off halfway. It gives up after drainWait (the cut-off runs are
// re-run on start, as before). A daemon too old to drain, or not running, is restarted at once.
func drainBeforeRestart() {
	c, err := newClient()
	if err != nil {
		return
	}
	c.http.Timeout = 5 * time.Second
	var st drainState
	if err := c.call("POST", "/api/drain", map[string]bool{"on": true}, &st); err != nil {
		fmt.Printf("没法让守护进程先排空（%v），直接重启\n", err)
		return
	}
	// Ctrl-C while waiting: let the daemon carry on as if nothing happened.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	deadline := time.Now().Add(drainWait)
	last := ""
	for len(st.Running) > 0 {
		if line := describeRunning(st.Running); line != last {
			fmt.Printf("等待进行中的 %d 轮结束后再重启：%s\n", len(st.Running), line)
			last = line
		}
		if time.Now().After(deadline) {
			fmt.Printf("等了 %s 还没结束，直接重启（被打断的轮次启动后会自动补跑）\n", drainWait)
			return
		}
		select {
		case <-sig:
			_ = c.call("POST", "/api/drain", map[string]bool{"on": false}, nil)
			fmt.Println("已取消安装，守护进程照常运行")
			os.Exit(130)
		case <-time.After(drainPoll):
		}
		if err := c.call("GET", "/api/drain", nil, &st); err != nil {
			fmt.Printf("查询运行状态失败（%v），直接重启\n", err)
			return
		}
	}
	if st.Pending > 0 {
		fmt.Printf("没有进行中的轮次了；排队的 %d 个请求重启后接着跑\n", st.Pending)
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
