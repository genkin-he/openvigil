// Package health checks the logins Vigil's tasks depend on (the claude CLI, Slack, whatever CLIs and MCP servers you add) and
// raises a to-do before a task fails on an expired credential. It runs plain commands, no agent.
package health

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"time"

	"vigil/internal/proc"
)

// Check is one login to verify. Command exits 0 when healthy; its output explains a failure.
type Check struct {
	Name    string `yaml:"name" json:"name"`
	Command string `yaml:"command" json:"command"`
	Fix     string `yaml:"fix,omitempty" json:"fix,omitempty"`   // tried once, unattended, before raising a to-do
	Hint    string `yaml:"hint,omitempty" json:"hint,omitempty"` // what the owner should do
}

// Result is the outcome of one check.
type Result struct {
	Name    string    `json:"name"`
	OK      bool      `json:"ok"`
	Fixed   bool      `json:"fixed,omitempty"` // broken, repaired by Fix
	Output  string    `json:"output,omitempty"`
	Checked time.Time `json:"checked_at"`
}

// Ref is the to-do ref for a check, so repeated failures update one item.
func Ref(name string) string { return "check:" + name }

const (
	commandTimeout = 2 * time.Minute
	maxOutput      = 1500
)

// Exec runs a shell command; replaceable in tests.
type Exec func(ctx context.Context, command string) (string, error)

// Shell runs command with bash, bounded by commandTimeout.
func Shell(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := osexec.CommandContext(ctx, "/bin/bash", "-c", command)
	cmd.Env = os.Environ()
	proc.Prepare(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Checker runs the checks and keeps the to-dos in step: raise on failure, resolve on recovery.
type Checker struct {
	Checks  []Check
	Exec    Exec
	Raise   func(c Check, output string) error // open or refresh the to-do for a failing check
	Resolve func(c Check) error                // close the to-do of a check that passes again
	Logf    func(string, ...any)
	Now     func() time.Time

	mu   sync.Mutex
	last map[string]Result
}

// Run checks everything once and returns the results in check order.
func (k *Checker) Run(ctx context.Context) []Result {
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}
	run := k.Exec
	if run == nil {
		run = Shell
	}
	out := make([]Result, 0, len(k.Checks))
	for _, c := range k.Checks {
		if ctx.Err() != nil {
			break
		}
		r := k.one(ctx, run, c)
		if ctx.Err() != nil {
			break // cut off by shutdown: the failure is ours, not the login's
		}
		r.Checked = now()
		out = append(out, r)
		k.settle(c, r)
	}
	k.mu.Lock()
	k.last = make(map[string]Result, len(out))
	for _, r := range out {
		k.last[r.Name] = r
	}
	k.mu.Unlock()
	return out
}

func (k *Checker) one(ctx context.Context, run Exec, c Check) Result {
	output, err := run(ctx, c.Command)
	if err == nil {
		return Result{Name: c.Name, OK: true}
	}
	if strings.TrimSpace(c.Fix) != "" && ctx.Err() == nil {
		fixOut, fixErr := run(ctx, c.Fix)
		k.logf("check %s failed, tried fix: %v %s", c.Name, fixErr, clip(fixOut, 200))
		if again, err := run(ctx, c.Command); err == nil {
			return Result{Name: c.Name, OK: true, Fixed: true}
		} else {
			output = again
		}
	}
	return Result{Name: c.Name, Output: clip(strings.TrimSpace(output+"\n"+errText(err)), maxOutput)}
}

func (k *Checker) settle(c Check, r Result) {
	var err error
	if r.OK {
		if k.Resolve != nil {
			err = k.Resolve(c)
		}
	} else {
		k.logf("check %s failing: %s", c.Name, clip(r.Output, 200))
		if k.Raise != nil {
			err = k.Raise(c, r.Output)
		}
	}
	if err != nil {
		k.logf("check %s: update to-do: %v", c.Name, err)
	}
}

// Last returns the most recent results by check name.
func (k *Checker) Last() map[string]Result {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]Result, len(k.last))
	for n, r := range k.last {
		out[n] = r
	}
	return out
}

// Loop runs the checks after first, then every interval, and whenever kick receives.
func (k *Checker) Loop(ctx context.Context, first, interval time.Duration, kick <-chan struct{}) {
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-timer.C:
		}
		k.Run(ctx)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
	}
}

func (k *Checker) logf(f string, a ...any) {
	if k.Logf != nil {
		k.Logf(f, a...)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("（%v）", err)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
