// Package proc runs child processes in their own process group and makes sure they can always be stopped.
package proc

import (
	"bufio"
	"context"
	"errors"
	"io"
	osexec "os/exec"
	"syscall"
	"time"
)

// KillGrace is how long a process group gets between SIGTERM and SIGKILL.
const KillGrace = 5 * time.Second

// Prepare puts cmd in its own process group; cancelling its context sends SIGTERM to the whole group.
func Prepare(cmd *osexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = KillGrace
}

// Guard must be called after cmd.Start. Once ctx is done it waits KillGrace, then SIGKILLs the group and
// closes pipe, so a reader blocked on a pipe held open by an escaped grandchild is released.
// The returned stop function ends the guard after the command finished normally.
func Guard(ctx context.Context, cmd *osexec.Cmd, pipe io.Closer) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		select {
		case <-done:
			return
		case <-time.After(KillGrace):
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if pipe != nil {
			_ = pipe.Close()
		}
	}()
	return func() { close(done) }
}

// Lines calls fn for every line of r, whatever its length, until EOF or a read error.
func Lines(r io.Reader, fn func(line []byte)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(trimEOL(line))
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
