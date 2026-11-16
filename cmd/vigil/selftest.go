package main

import (
	"fmt"
	"time"

	"vigil/internal/server"
)

const (
	selfTestPollCLI = 5 * time.Second
	daemonUpWait    = 30 * time.Second
)

// cmdSelfTest runs the end-to-end self-test and prints each step as it finishes.
func cmdSelfTest(args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var rep server.SelfTestReport
	if hasFlag(args, "--status") {
		if err := c.call("GET", "/api/selftest", nil, &rep); err != nil {
			return err
		}
		printSelfTest(rep, 0)
		return nil
	}
	if err := c.call("POST", "/api/selftest", nil, &rep); err != nil {
		return err
	}
	fmt.Printf("自测 %s 开始（Slack 自聊里会出现一条自测消息，约几分钟）\n", rep.ID)
	shown := 0
	for rep.Status == "running" {
		time.Sleep(selfTestPollCLI)
		if err := c.call("GET", "/api/selftest", nil, &rep); err != nil {
			return err
		}
		shown = printSelfTest(rep, shown)
	}
	if rep.Status != "pass" {
		return fmt.Errorf("自测没通过，已在待办里记了一条")
	}
	fmt.Println("自测全部通过")
	return nil
}

func printSelfTest(rep server.SelfTestReport, from int) int {
	if from == 0 && rep.Status != "running" {
		fmt.Printf("自测 %s：%s（%s）\n", rep.ID, rep.Status, rep.Started.Format("01-02 15:04"))
	}
	for _, s := range rep.Steps[from:] {
		mark := "✓"
		if !s.OK {
			mark = "✗"
		}
		fmt.Printf("  %s %s（%s）%s\n", mark, s.Name, s.Took, map[bool]string{true: "", false: "：" + s.Detail}[s.OK])
	}
	return len(rep.Steps)
}

// selfTestAfterInstall starts a background self-test once the new daemon answers (skipped when one
// passed recently); the result shows up as a to-do only when something is broken.
func selfTestAfterInstall() {
	c, err := newClient()
	if err != nil {
		return
	}
	c.http.Timeout = 3 * time.Second
	deadline := time.Now().Add(daemonUpWait)
	for {
		var rep server.SelfTestReport
		err := c.call("POST", "/api/selftest?auto=1", nil, &rep)
		if err == nil {
			if rep.Status == "running" {
				fmt.Println("端到端自测已在后台开始，没通过会出现在待办里（vigil selftest --status 查看）")
			}
			return
		}
		if time.Now().After(deadline) {
			fmt.Printf("没能启动自测：%v\n", err)
			return
		}
		time.Sleep(time.Second)
	}
}
