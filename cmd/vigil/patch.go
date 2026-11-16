package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	osexec "os/exec"
	"syscall"

	"vigil/internal/patch"
)

// cmdPatch hands in a change to Vigil's own code for the owner to approve in the console.
func cmdPatch(args []string) error {
	if len(args) == 0 || args[0] != "propose" {
		return errors.New(commands["patch"].usage)
	}
	fs := flag.NewFlagSet("patch propose", flag.ContinueOnError)
	var b struct {
		Task  string `json:"task"`
		Title string `json:"title"`
		Why   string `json:"why"`
		Diff  string `json:"diff"`
	}
	fs.StringVar(&b.Task, "task", "", "")
	fs.StringVar(&b.Title, "title", "", "")
	fs.StringVar(&b.Why, "why", "", "")
	file := fs.String("f", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	diff, err := readFile(*file)
	if err != nil || diff == nil {
		return fmt.Errorf("需要 -f <git diff 文件>：%v", err)
	}
	b.Diff = *diff
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct {
		Patch struct {
			ID string `json:"id"`
		} `json:"patch"`
		Todo string `json:"todo"`
	}
	if err := c.call("POST", "/api/patches", b, &out); err != nil {
		return err
	}
	fmt.Printf("已提交补丁 %s（待办 %s），等主人在控制台看过 diff 后确认；确认后自动测试、合并、安装\n", out.Patch.ID, out.Todo)
	return nil
}

// startInstall runs `make install` in repo in a session of its own, so it survives the daemon restart it
// causes, and writes its log and exit code where the patch store expects them.
func startInstall(repo string, st patch.Store) func(id string) error {
	return func(id string) error {
		f, err := os.OpenFile(st.InstallLog(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd := osexec.Command("/bin/bash", "-c", `make install; echo $? > "$1"`, "install", st.InstallExit(id))
		cmd.Dir, cmd.Stdout, cmd.Stderr = repo, f, f
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
}
