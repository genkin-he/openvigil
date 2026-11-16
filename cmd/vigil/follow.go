package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
)

// cmdFollow registers and settles what an agent waits for from other people.
func cmdFollow(args []string) error {
	if len(args) == 0 {
		return errors.New(commands["follow"].usage)
	}
	switch args[0] {
	case "add":
		return followAdd(args[1:])
	case "list":
		if len(args) > 1 && args[1] == "--all" {
			return getJSON("/api/follows?all=1")
		}
		return getJSON("/api/follows")
	case "done":
		return followUpdate(args[1:], "done")
	case "snooze":
		return followUpdate(args[1:], "snooze")
	}
	return errors.New(commands["follow"].usage)
}

func followAdd(args []string) error {
	fs := flag.NewFlagSet("follow add", flag.ContinueOnError)
	b := map[string]*string{}
	for _, k := range []string{"task", "title", "who", "ask", "link", "ref", "due"} {
		b[k] = fs.String(k, "", "")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	body := map[string]string{}
	for k, v := range b {
		body[k] = *v
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct {
		ID  string `json:"id"`
		Due string `json:"due"`
	}
	if err := c.call("POST", "/api/follows", body, &out); err != nil {
		return err
	}
	fmt.Printf("已登记跟进 %s，%s 到期\n", out.ID, out.Due)
	return nil
}

func followUpdate(args []string, action string) error {
	if len(args) == 0 {
		return fmt.Errorf("vigil follow %s <fu-id> …", action)
	}
	id := args[0]
	fs := flag.NewFlagSet("follow "+action, flag.ContinueOnError)
	outcome := fs.String("outcome", "", "")
	due := fs.String("due", "", "")
	nudged := fs.Bool("nudged", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	body := map[string]any{"outcome": *outcome, "due": *due, "nudged": *nudged}
	if err := c.call("POST", "/api/follows/"+url.PathEscape(id)+"/"+action, body, nil); err != nil {
		return err
	}
	fmt.Printf("%s：%s\n", id, action)
	return nil
}
