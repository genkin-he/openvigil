package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vigil/internal/exec"
	"vigil/internal/task"
)

const (
	producerPoll       = 5 * time.Second
	producerMinBackoff = 2 * time.Second
	producerMaxBackoff = time.Minute
	producerStopWait   = 5 * time.Second
	producerNotifyAt   = 5
)

// Producer keeps a stream's producer process alive. The process runs in its own session so it
// survives Vigil restarts; a running one is adopted through its pidfile instead of being replaced.
type Producer struct {
	Spec   task.Producer
	Dir    string
	Logf   func(string, ...any)
	OnFail func(err error, streak int) // repeated start failures
	Sleep  func(ctx context.Context, d time.Duration)
}

func (p Producer) pidfile() string { return exec.ExpandHome(p.Spec.PIDFile) }

// alive reports the pid recorded in the pidfile when that process is still running and matches.
func (p Producer) alive() (int, bool) {
	data, err := os.ReadFile(p.pidfile())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	if syscall.Kill(pid, 0) != nil {
		return 0, false
	}
	if p.Spec.Match != "" {
		out, err := osexec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if err != nil || !strings.Contains(string(out), p.Spec.Match) {
			return 0, false
		}
	}
	return pid, true
}

func (p Producer) start() (int, error) {
	cmd := osexec.Command("/bin/bash", "-c", p.Spec.Command)
	cmd.Dir = p.Dir
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("open devnull: %w", err)
	}
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start producer: %w", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap it if it exits while Vigil runs
	if err := os.WriteFile(p.pidfile(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return pid, fmt.Errorf("write pidfile: %w", err)
	}
	return pid, nil
}

func (p Producer) sleep(ctx context.Context, d time.Duration) {
	if p.Sleep != nil {
		p.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Run supervises the producer until ctx ends. Ending ctx leaves the producer running (Vigil restart);
// call Stop to shut it down (task paused or removed).
func (p Producer) Run(ctx context.Context) {
	backoff := producerMinBackoff
	streak := 0
	for ctx.Err() == nil {
		if pid, ok := p.alive(); ok {
			if streak > 0 {
				p.Logf("producer running again (pid %d)", pid)
			}
			backoff, streak = producerMinBackoff, 0
			p.sleep(ctx, producerPoll)
			continue
		}
		pid, err := p.start()
		if err == nil {
			p.Logf("producer started (pid %d): %s", pid, p.Spec.Command)
			p.sleep(ctx, producerPoll)
			if _, ok := p.alive(); ok {
				continue
			}
			err = errors.New("producer exited right after start")
		}
		streak++
		p.Logf("producer failed (%d in a row): %v", streak, err)
		if streak == producerNotifyAt && p.OnFail != nil {
			p.OnFail(err, streak)
		}
		p.sleep(ctx, backoff)
		backoff = min(backoff*2, producerMaxBackoff)
	}
}

// Stop terminates the producer's process group.
func (p Producer) Stop() {
	pid, ok := p.alive()
	if !ok {
		return
	}
	signal := func(sig syscall.Signal) {
		// a producer started by hand (nohup) may not lead its own group: fall back to the process
		if syscall.Kill(-pid, sig) != nil {
			_ = syscall.Kill(pid, sig)
		}
	}
	signal(syscall.SIGTERM)
	deadline := time.Now().Add(producerStopWait)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	signal(syscall.SIGKILL)
}
