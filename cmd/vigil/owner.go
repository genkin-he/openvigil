package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// cmdOwner shows or changes the owner's principles, which every task reads.
func cmdOwner(args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "show" {
		var doc struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := c.call("GET", "/api/owner", nil, &doc); err != nil {
			return err
		}
		if strings.TrimSpace(doc.Content) == "" {
			fmt.Println("还没有判断原则（" + doc.Path + "）")
			return nil
		}
		fmt.Print(doc.Content)
		return nil
	}
	var out map[string]any
	switch args[0] {
	case "append":
		if len(args) < 2 {
			return errors.New(commands["owner"].usage)
		}
		err = c.call("POST", "/api/owner/principles", map[string]string{"text": strings.Join(args[1:], " ")}, &out)
	case "set":
		fs := flag.NewFlagSet("owner set", flag.ContinueOnError)
		file := fs.String("f", "", "")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		content, rerr := readFile(*file)
		if rerr != nil || content == nil {
			return fmt.Errorf("需要 -f <原则文件>：%v", rerr)
		}
		err = c.call("PUT", "/api/owner", map[string]string{"content": *content}, &out)
	default:
		return errors.New(commands["owner"].usage)
	}
	if err != nil {
		return err
	}
	if hint, ok := out["hint"].(string); ok {
		fmt.Println(hint)
	}
	return nil
}
