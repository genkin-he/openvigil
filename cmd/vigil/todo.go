package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"vigil/internal/inbox"
)

const todoUsage = `vigil todo add --task <任务id> --title "<人>：<一句话>" [--kind <类别>] [--detail ..] [--analysis ..|-] [--reply ..|--shadow ..] [--link ..] [--ref ..] [--target ..] [--owner] [--choice "<按钮>|<后果>" ...]
vigil todo list [--all]
vigil todo kinds <任务id>          已用过的类别和你的处理方式
vigil todo done <id> --outcome "<实际发出的原文或做了什么>" [--by-owner]   --by-owner：主人自己已经处理了（网页批了、Slack 亲手回了）
vigil todo note <id> --analysis "<补充>"|- [--reopen]`

// cmdTodo manages the owner's to-dos. Agents raise them; the owner answers in the console.
func cmdTodo(args []string) error {
	if len(args) == 0 {
		return errors.New(todoUsage)
	}
	switch args[0] {
	case "add":
		return todoAdd(args[1:])
	case "list", "ls":
		return todoList(args[1:])
	case "done":
		return todoDone(args[1:])
	case "note":
		return todoNoteCmd(args[1:])
	case "kinds":
		return todoKinds(args[1:])
	default:
		return errors.New(todoUsage)
	}
}

func todoAdd(args []string) error {
	fs := flag.NewFlagSet("todo add", flag.ContinueOnError)
	var it inbox.Item
	fs.StringVar(&it.TaskID, "task", "", "")
	fs.StringVar(&it.Title, "title", "", "")
	fs.StringVar(&it.Detail, "detail", "", "")
	analysis := fs.String("analysis", "", "")
	fs.StringVar(&it.Reply, "reply", "", "")
	fs.StringVar(&it.Shadow, "shadow", "", "")
	fs.StringVar(&it.Link, "link", "", "")
	fs.StringVar(&it.Ref, "ref", "", "")
	fs.StringVar(&it.Target, "target", "", "")
	fs.StringVar(&it.Kind, "kind", "", "")
	fs.BoolVar(&it.Owner, "owner", false, "")
	fs.Func("choice", "", func(v string) error {
		label, note, _ := strings.Cut(v, "|")
		it.Choices = append(it.Choices, inbox.Choice{Label: strings.TrimSpace(label), Note: strings.TrimSpace(note)})
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	text, err := textOrStdin(*analysis)
	if err != nil {
		return err
	}
	it.Analysis = text
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct {
		Item    inbox.Item `json:"item"`
		Created bool       `json:"created"`
	}
	if err := c.call("POST", "/api/todos", it, &out); err != nil {
		return err
	}
	verb := "已更新"
	if out.Created {
		verb = "已新建"
	}
	fmt.Printf("%s待办 %s：%s\n", verb, out.Item.ID, out.Item.Title)
	return nil
}

func todoList(args []string) error {
	fs := flag.NewFlagSet("todo list", flag.ContinueOnError)
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	path := "/api/todos"
	if *all {
		path += "?all=1"
	}
	var items []inbox.Item
	if err := c.call("GET", path, nil, &items); err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("没有待办")
		return nil
	}
	for _, it := range items {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", it.ID, it.Status, it.TaskID, it.Ref, it.Title)
	}
	return nil
}

// todoKinds lists the kinds a task has used, so agents reuse them instead of inventing new ones.
func todoKinds(args []string) error {
	if len(args) == 0 {
		return errors.New(todoUsage)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var items []inbox.Item
	if err := c.call("GET", "/api/todos?all=1", nil, &items); err != nil {
		return err
	}
	type tally struct {
		n         int
		decisions map[string]int
	}
	kinds := map[string]*tally{}
	var order []string
	for _, it := range items {
		if it.TaskID != args[0] || it.Kind == "" {
			continue
		}
		k := kinds[it.Kind]
		if k == nil {
			k = &tally{decisions: map[string]int{}}
			kinds[it.Kind] = k
			order = append(order, it.Kind)
		}
		k.n++
		if it.Decision != "" {
			k.decisions[it.Decision]++
		}
	}
	if len(order) == 0 {
		fmt.Println("还没有类别")
		return nil
	}
	label := map[string]string{inbox.DecisionAsIs: "原样发", inbox.DecisionEdited: "改了再发",
		inbox.DecisionInstructed: "给了指示", inbox.DecisionDismissed: "不用管"}
	for _, kind := range order {
		k := kinds[kind]
		var parts []string
		for _, d := range []string{inbox.DecisionAsIs, inbox.DecisionEdited, inbox.DecisionInstructed, inbox.DecisionDismissed} {
			if k.decisions[d] > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", label[d], k.decisions[d]))
			}
		}
		fmt.Printf("%s\t%d 条\t%s\n", kind, k.n, strings.Join(parts, "，"))
	}
	return nil
}

func todoDone(args []string) error {
	if len(args) == 0 {
		return errors.New(todoUsage)
	}
	fs := flag.NewFlagSet("todo done", flag.ContinueOnError)
	outcome := fs.String("outcome", "", "")
	byOwner := fs.Bool("by-owner", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	body := map[string]any{"outcome": *outcome, "by_owner": *byOwner}
	if err := c.call("POST", "/api/todos/"+args[0]+"/done", body, nil); err != nil {
		return err
	}
	fmt.Println("待办 " + args[0] + " 已完成")
	return nil
}

func todoNoteCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(todoUsage)
	}
	fs := flag.NewFlagSet("todo note", flag.ContinueOnError)
	analysis := fs.String("analysis", "", "")
	reopen := fs.Bool("reopen", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	text, err := textOrStdin(*analysis)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("--analysis 不能为空")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.call("POST", "/api/todos/"+args[0]+"/note", map[string]any{"analysis": text, "reopen": *reopen}, nil); err != nil {
		return err
	}
	fmt.Println("待办 " + args[0] + " 已补充")
	return nil
}

// textOrStdin reads stdin when the flag value is "-", so multi-line text needs no shell quoting.
func textOrStdin(v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(data), nil
}

// cmdSpawn hands a long piece of work to a background job of a task (agents call it mid-run).
func cmdSpawn(args []string) error {
	id, rest, err := needID(args, "spawn")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("spawn", flag.ContinueOnError)
	title := fs.String("title", "", "")
	input := fs.String("input", "", "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	text, err := textOrStdin(*input)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct {
		Job   string `json:"job"`
		Title string `json:"title"`
	}
	if err := c.call("POST", "/api/tasks/"+id+"/jobs", map[string]string{"title": *title, "input": text}, &out); err != nil {
		return err
	}
	fmt.Printf("已转后台子任务 %s：%s\n", out.Job, out.Title)
	return nil
}
