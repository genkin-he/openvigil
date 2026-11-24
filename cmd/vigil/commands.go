package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"vigil/internal/config"
	"vigil/internal/server"
	"vigil/internal/store"
)

const waitPoll = 3 * time.Second

func needID(args []string, cmd string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("用法：%s", commands[cmd].usage)
	}
	return args[0], args[1:], nil
}

type overview struct {
	Tasks    []server.TaskView `json:"tasks"`
	Problems []string          `json:"problems"`
}

func cmdList(_ []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var ov overview
	if err := c.call("GET", "/api/overview", nil, &ov); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\t状态\t模式\t触发\t下次\t最近结果")
	for _, v := range ov.Tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.Task.ID, stateLabel(v), v.Task.Mode, triggerLabel(v), nextLabel(v), lastLabel(v))
	}
	tw.Flush()
	for _, p := range ov.Problems {
		fmt.Println("⚠ " + p)
	}
	return nil
}

func stateLabel(v server.TaskView) string {
	switch {
	case v.Live != nil:
		return "◐ 运行中"
	case !v.Task.Enabled:
		return "‖ 暂停"
	case v.NeedsYou != nil:
		return "◆ 需要你"
	}
	return "● 值守"
}

func triggerLabel(v server.TaskView) string {
	t := v.Task.Trigger
	switch t.Kind {
	case "cron":
		return "cron " + t.Cron
	case "every":
		return "每 " + t.Every.String()
	case "once":
		return "一次 " + t.At.Local().Format("01-02 15:04")
	}
	return t.Kind
}

func nextLabel(v server.TaskView) string {
	if v.NextRun == nil {
		return "-"
	}
	return v.NextRun.Local().Format("01-02 15:04")
}

func lastLabel(v server.TaskView) string {
	if v.LastRun == nil {
		return "-"
	}
	return v.LastRun.StartedAt.Local().Format("15:04") + " " + v.LastRun.Status + " " + oneLine(v.LastRun.Summary, 60)
}

func oneLine(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

type detail struct {
	View server.TaskView   `json:"view"`
	Runs []store.RunRecord `json:"runs"`
	Docs map[string]string `json:"docs"`
	YAML string            `json:"yaml"`
}

func getDetail(c *client, id string) (detail, error) {
	var d detail
	return d, c.call("GET", "/api/tasks/"+id, nil, &d)
}

func cmdShow(args []string) error {
	id, _, err := needID(args, "show")
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	d, err := getDetail(c, id)
	if err != nil {
		return err
	}
	fmt.Printf("%s（%s）· %s · %s\n\n%s\n", d.View.Task.Name, id, stateLabel(d.View), triggerLabel(d.View), d.YAML)
	fmt.Println("── brief ──\n" + strings.TrimSpace(d.Docs["brief"]))
	printRuns(d.Runs, 5)
	return nil
}

func printRuns(runs []store.RunRecord, n int) {
	fmt.Println("\n── 最近几轮 ──")
	for i, r := range runs {
		if i == n {
			break
		}
		fmt.Printf("%s  %-9s %-8s %s\n", r.StartedAt.Local().Format("01-02 15:04"), r.Status, r.Trigger, oneLine(r.Summary, 100))
		if r.Question != "" {
			fmt.Printf("    ↳ %s %s\n", r.Question, strings.Join(r.Options, " / "))
		}
	}
}

func cmdLogs(args []string) error {
	id, rest, err := needID(args, "logs")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	n := fs.Int("n", 20, "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	d, err := getDetail(c, id)
	if err != nil {
		return err
	}
	printRuns(d.Runs, *n)
	return nil
}

func cmdStatus(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("控制台：%s\n数据目录：%s\nclaude：%s\nherdr：%s（session %s）\n\n", cfg.URL(), cfg.Root, cfg.ClaudeBin, cfg.HerdrBin, cfg.HerdrSession)
	return cmdList(nil)
}

func cmdWeb(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return osexec.Command("open", cfg.URL()).Run()
}

func cmdNew(args []string) error {
	idea := strings.TrimSpace(strings.Join(args, " "))
	if idea == "" {
		return errors.New(commands["new"].usage)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct{ Name string }
	if err := c.call("POST", "/api/forge", map[string]string{"idea": idea}, &out); err != nil {
		return err
	}
	fmt.Printf("锻造会话 %s 已在 herdr 里打开（workspace「Vigil 锻造」），去那里和它对话；也可以在控制台的锻造台里聊。\n", out.Name)
	return nil
}

func readFile(path string) (*string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	s := string(data)
	return &s, nil
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	file := fs.String("f", "", "")
	brief := fs.String("brief", "", "")
	runbook := fs.String("runbook", "", "")
	forgeName := fs.String("forge", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New(commands["apply"].usage)
	}
	yaml, err := readFile(*file)
	if err != nil {
		return err
	}
	b, err := readFile(*brief)
	if err != nil {
		return err
	}
	rb, err := readFile(*runbook)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct {
		ID      string
		Created bool
	}
	if err := c.call("POST", "/api/tasks", map[string]any{"yaml": *yaml, "brief": b, "runbook": rb, "forge": *forgeName}, &out); err != nil {
		return err
	}
	fmt.Printf("%s 任务 %s\n", map[bool]string{true: "已创建", false: "已更新"}[out.Created], out.ID)
	return nil
}

func cmdRun(args []string) error {
	id, rest, err := needID(args, "run")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	input := fs.String("input", "", "")
	wait := fs.Bool("wait", false, "")
	trial := fs.Bool("trial", false, "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	before, err := getDetail(c, id)
	if err != nil {
		return err
	}
	if err := c.call("POST", "/api/tasks/"+id+"/run", map[string]any{"input": *input, "trial": *trial}, nil); err != nil {
		return err
	}
	if !*wait {
		fmt.Println("已加入队列：" + id)
		return nil
	}
	return waitRun(c, id, latestID(before.Runs))
}

func latestID(runs []store.RunRecord) string {
	if len(runs) == 0 {
		return ""
	}
	return runs[0].ID
}

func waitRun(c *client, id, prev string) error {
	fmt.Fprint(os.Stderr, "等待这一轮跑完")
	for {
		time.Sleep(waitPoll)
		d, err := getDetail(c, id)
		if err != nil {
			return err
		}
		if len(d.Runs) > 0 && d.Runs[0].ID != prev && d.Runs[0].Status != store.StatusRunning {
			r := d.Runs[0]
			fmt.Fprintln(os.Stderr)
			fmt.Printf("状态：%s\n结果：%s\n", r.Status, r.Summary)
			if r.Question != "" {
				fmt.Printf("问题：%s\n选项：%s\n推荐：%s\n", r.Question, strings.Join(r.Options, " / "), r.Recommendation)
			}
			return nil
		}
		fmt.Fprint(os.Stderr, ".")
	}
}

func simplePost(cmd, suffix string, args []string, body any) error {
	id, _, err := needID(args, cmd)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out map[string]any
	if err := c.call("POST", "/api/tasks/"+id+suffix, body, &out); err != nil {
		return err
	}
	if hint, ok := out["hint"].(string); ok {
		fmt.Println(hint)
	} else {
		fmt.Println("好了：" + id)
	}
	return nil
}

func cmdPause(args []string) error  { return simplePost("pause", "/pause", args, nil) }
func cmdResume(args []string) error { return simplePost("resume", "/resume", args, nil) }
func cmdOpen(args []string) error   { return simplePost("open", "/open", args, nil) }

func cmdRemove(args []string) error {
	id, _, err := needID(args, "rm")
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.call("DELETE", "/api/tasks/"+id, nil, nil); err != nil {
		return err
	}
	fmt.Println("已删除：" + id)
	return nil
}

func cmdAnswer(args []string) error {
	if len(args) < 2 {
		return errors.New(commands["answer"].usage)
	}
	return simplePost("answer", "/answer", args[:1], map[string]string{"text": strings.Join(args[1:], " ")})
}

func cmdMemory(args []string) error {
	if len(args) < 3 || args[1] != "append" {
		return errors.New(commands["memory"].usage)
	}
	text, topic := memoryText(args[2:])
	if topic != "" {
		text = "[" + topic + "] " + text // a fact: filed under its topic, not in memory.md
	}
	return simplePost("memory", "/memory", args[:1], map[string]string{"text": text})
}

// memoryText reads "[--topic <topic>] text…".
func memoryText(args []string) (text, topic string) {
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--topic" && i+1 < len(args) {
			topic = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return strings.Join(rest, " "), topic
}

func cmdAmend(args []string) error {
	id, rest, err := needID(args, "amend")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("amend", flag.ContinueOnError)
	file := fs.String("f", "", "")
	reason := fs.String("reason", "", "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	content, err := readFile(*file)
	if err != nil || content == nil {
		return fmt.Errorf("需要 -f <runbook 文件>：%v", err)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out map[string]string
	if err := c.call("POST", "/api/tasks/"+id+"/amend", map[string]string{"content": *content, "reason": *reason}, &out); err != nil {
		return err
	}
	if out["applied"] == "true" {
		fmt.Println("runbook 已更新")
	} else {
		fmt.Println("已提交，等主人在控制台批准：" + out["proposal"])
	}
	return nil
}

func cmdDone(args []string) error {
	if len(args) < 1 {
		return errors.New(commands["done"].usage)
	}
	runID := args[0]
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	js := fs.String("json", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	body := []byte(*js)
	if *js == "" || *js == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		body = data
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.callRaw("/api/runs/"+runID+"/done", body); err != nil {
		return err
	}
	fmt.Println("已回报")
	return nil
}
