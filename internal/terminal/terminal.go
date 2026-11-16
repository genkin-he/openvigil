// Package terminal finds and raises the macOS app that hosts the owner's herdr client.
package terminal

import (
	"context"
	"errors"
	"fmt"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxDepth   = 12
	appMarker  = ".app/Contents/MacOS/"
	cmdTimeout = 5 * time.Second
)

// Proc is one row of the process table.
type Proc struct {
	PID  int
	PPID int
	Args string
}

// Lister returns the process table; replaceable in tests.
type Lister func(ctx context.Context) ([]Proc, error)

// ListProcs reads the process table with ps.
func ListProcs(ctx context.Context) ([]Proc, error) {
	out, err := osexec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs = append(procs, Proc{PID: pid, PPID: ppid, Args: strings.Join(f[2:], " ")})
	}
	return procs, nil
}

// isClient reports whether args belong to an attached herdr UI client for the given session.
func isClient(args, session string) bool {
	f := strings.Fields(args)
	if len(f) == 0 || filepath.Base(f[0]) != "herdr" {
		return false
	}
	rest := f[1:]
	if session == "" || session == "default" {
		return len(rest) == 0
	}
	return len(rest) == 2 && rest[0] == "--session" && rest[1] == session
}

// FindApp returns the .app bundle path of the terminal that hosts the herdr client of session.
func FindApp(procs []Proc, session string) (string, error) {
	byPID := make(map[int]Proc, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	for _, p := range procs {
		if !isClient(p.Args, session) {
			continue
		}
		cur := p
		for i := 0; i < maxDepth && cur.PPID > 1; i++ {
			parent, ok := byPID[cur.PPID]
			if !ok {
				break
			}
			if i := strings.Index(parent.Args, appMarker); i >= 0 {
				return parent.Args[:i+len(".app")], nil
			}
			cur = parent
		}
	}
	return "", errors.New("没找到运行 herdr 的终端程序")
}

// Raise brings the terminal app to the front. app may be a configured app name or bundle path;
// when empty it is detected from the running herdr client.
func Raise(ctx context.Context, app, session string, list Lister) error {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	if app == "" {
		procs, err := list(ctx)
		if err != nil {
			return err
		}
		if app, err = FindApp(procs, session); err != nil {
			return err
		}
	}
	if out, err := osexec.CommandContext(ctx, "open", "-a", app).CombinedOutput(); err != nil {
		return fmt.Errorf("open -a %s: %w: %s", app, err, strings.TrimSpace(string(out)))
	}
	return nil
}
