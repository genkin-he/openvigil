package stream

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"vigil/internal/proc"
)

const (
	minBackoff   = time.Second
	maxBackoff   = 60 * time.Second
	healthyAfter = 60 * time.Second
)

// Source keeps a shell command running and hands each stdout line to OnLine.
type Source struct {
	Command string
	Dir     string
	OnLine  func(line string)
	// OnExit is called after each exit with the error and how many exits happened in a row without a healthy run.
	OnExit func(err error, streak int)
	Sleep  func(ctx context.Context, d time.Duration)
}

// Run blocks until ctx is cancelled, restarting the command with exponential backoff.
func (s Source) Run(ctx context.Context) {
	backoff := minBackoff
	streak := 0
	for ctx.Err() == nil {
		started := time.Now()
		err := s.once(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= healthyAfter {
			backoff, streak = minBackoff, 0
		}
		streak++
		if s.OnExit != nil {
			s.OnExit(err, streak)
		}
		s.sleep(ctx, backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}

func (s Source) sleep(ctx context.Context, d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (s Source) once(ctx context.Context) error {
	cmd := osexec.CommandContext(ctx, "/bin/bash", "-c", s.Command)
	cmd.Dir = s.Dir
	cmd.Env = os.Environ()
	proc.Prepare(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &capped{b: &stderr, n: 4096}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start stream: %w", err)
	}
	stop := proc.Guard(ctx, cmd, stdout)
	defer stop()
	readErr := proc.Lines(stdout, func(b []byte) {
		if line := strings.TrimSpace(string(b)); line != "" {
			s.OnLine(line)
		}
	})
	err = cmd.Wait()
	if err == nil && readErr != nil {
		err = readErr
	}
	if err != nil {
		return fmt.Errorf("stream exited: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("stream exited normally")
}

type capped struct {
	b *strings.Builder
	n int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.n > 0 {
		chunk := p
		if len(chunk) > c.n {
			chunk = chunk[:c.n]
		}
		c.n -= len(chunk)
		c.b.Write(chunk)
	}
	return len(p), nil
}
