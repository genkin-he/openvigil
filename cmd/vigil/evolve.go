package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// cmdEvolve serves the self-evolution task: the feedback since its last round, and the change ledger.
func cmdEvolve(args []string) error {
	if len(args) == 0 {
		return errors.New(commands["evolve"].usage)
	}
	switch args[0] {
	case "signals":
		return evolveSignals(args[1:])
	case "ledger":
		return getJSON("/api/evolve/ledger")
	case "record":
		return evolveRecord(args[1:])
	case "verdict":
		return evolveVerdict(args[1:])
	case "replay":
		return evolveReplay(args[1:])
	}
	return errors.New(commands["evolve"].usage)
}

func evolveSignals(args []string) error {
	fs := flag.NewFlagSet("evolve signals", flag.ContinueOnError)
	since := fs.String("since", "", "")
	task := fs.String("task", "", "只看这个任务的反馈、轮次、账本和北极星")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := url.Values{}
	if *since != "" {
		q.Set("since", *since)
	}
	if *task != "" {
		q.Set("task", *task)
	}
	path := "/api/evolve/signals"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return getJSON(path)
}

func evolveRecord(args []string) error {
	fs := flag.NewFlagSet("evolve record", flag.ContinueOnError)
	var b struct {
		Task       string `json:"task"`
		Target     string `json:"target"`
		Change     string `json:"change"`
		Why        string `json:"why"`
		Expect     string `json:"expect"`
		Backup     string `json:"backup"`
		Todo       string `json:"todo"`
		ReviewDays int    `json:"review_days"`
		Metric     string `json:"metric"`
	}
	fs.StringVar(&b.Task, "task", "", "")
	fs.StringVar(&b.Target, "target", "", "")
	fs.StringVar(&b.Change, "change", "", "")
	fs.StringVar(&b.Why, "why", "", "")
	fs.StringVar(&b.Expect, "expect", "", "")
	fs.StringVar(&b.Backup, "backup", "", "")
	fs.StringVar(&b.Todo, "todo", "", "")
	fs.IntVar(&b.ReviewDays, "review-days", 0, "")
	fs.StringVar(&b.Metric, "metric", "", "待办类别（或 <任务>/<类别>）：记下改动前 7 天的处理情况作为基线")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct {
		ID       string `json:"id"`
		ReviewAt string `json:"review_at"`
	}
	if err := c.call("POST", "/api/evolve/ledger", b, &out); err != nil {
		return err
	}
	fmt.Printf("已记账 %s，%s 复查\n", out.ID, out.ReviewAt)
	return nil
}

func evolveVerdict(args []string) error {
	if len(args) == 0 {
		return errors.New("vigil evolve verdict <ev-id> --keep|--revert --note \"依据\"")
	}
	id := args[0]
	fs := flag.NewFlagSet("evolve verdict", flag.ContinueOnError)
	keep := fs.Bool("keep", false, "")
	revert := fs.Bool("revert", false, "")
	note := fs.String("note", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *keep == *revert {
		return errors.New("--keep 和 --revert 选一个")
	}
	status := "kept"
	if *revert {
		status = "reverted"
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.call("POST", "/api/evolve/ledger/"+url.PathEscape(id), map[string]string{"status": status, "verdict": *note}, nil); err != nil {
		return err
	}
	fmt.Printf("%s：%s\n", id, status)
	return nil
}

// getJSON prints a GET endpoint's data as indented JSON.
func getJSON(path string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var out json.RawMessage
	if err := c.call("GET", path, nil, &out); err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// replayPoll is how often `vigil evolve replay` checks whether the replay has finished.
const (
	replayPoll = 3 * time.Second
	replayWait = 30 * time.Minute // give up waiting; the replay keeps its report either way
)

// evolveReplay replays a rule change on past rounds and prints the summary (it waits until done).
func evolveReplay(args []string) error {
	fs := flag.NewFlagSet("evolve replay", flag.ContinueOnError)
	var req struct {
		Task   string   `json:"task"`
		Doc    string   `json:"doc"`
		Before string   `json:"before"`
		After  string   `json:"after"`
		Kind   string   `json:"kind,omitempty"`
		Runs   []string `json:"runs,omitempty"`
	}
	fs.StringVar(&req.Task, "task", "", "")
	fs.StringVar(&req.Doc, "doc", "", "")
	fs.StringVar(&req.Kind, "kind", "", "")
	beforeFile := fs.String("before-file", "", "")
	afterFile := fs.String("after-file", "", "")
	runs := fs.String("runs", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if req.Task == "" || req.Doc == "" || *afterFile == "" {
		return errors.New("用法：vigil evolve replay --task <id> --doc <brief.md|runbook.md|memory.md|owner.md> --after-file <新文> [--before-file <原文>] [--kind <类别>] [--runs a,b]")
	}
	for _, f := range []struct {
		path string
		into *string
	}{{*beforeFile, &req.Before}, {*afterFile, &req.After}} {
		if f.path == "" {
			continue
		}
		data, err := os.ReadFile(f.path)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.path, err)
		}
		*f.into = string(data)
	}
	if *runs != "" {
		req.Runs = strings.Split(*runs, ",")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := c.call("POST", "/api/evolve/replay", req, &started); err != nil {
		return err
	}
	deadline := time.Now().Add(replayWait)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("回放 %s 超过 %s 还没跑完，先不等了；稍后用 GET /api/evolve/replay/%s 查看", started.ID, replayWait, started.ID)
		}
		var rep struct {
			Status, Summary, Error string
		}
		if err := c.call("GET", "/api/evolve/replay/"+started.ID, nil, &rep); err != nil {
			return err
		}
		switch rep.Status {
		case "done":
			fmt.Printf("%s\n报告：%s（控制台 /api/evolve/replay/%s）\n", rep.Summary, started.ID, started.ID)
			return nil
		case "failed":
			return fmt.Errorf("回放 %s 失败：%s", started.ID, rep.Error)
		}
		time.Sleep(replayPoll)
	}
}
