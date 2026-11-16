package proc

import (
	"context"
	osexec "os/exec"
	"strings"
	"testing"
	"time"
)

func TestLinesHandlesVeryLongLines(t *testing.T) {
	long := strings.Repeat("x", 10<<20)
	var got []int
	err := Lines(strings.NewReader("a\r\n"+long+"\nlast"), func(l []byte) { got = append(got, len(l)) })
	if err != nil || len(got) != 3 || got[0] != 1 || got[1] != len(long) || got[2] != 4 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestGuardReleasesPipeHeldByEscapedChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// The grandchild starts its own session (escapes the group) and keeps stdout open.
	cmd := osexec.CommandContext(ctx, "/bin/bash", "-c", "perl -e 'use POSIX; setsid(); sleep 60' & echo started; wait")
	Prepare(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := Guard(ctx, cmd, out)
	defer stop()
	start := time.Now()
	_ = Lines(out, func([]byte) {})
	_ = cmd.Wait()
	if time.Since(start) > KillGrace+3*time.Second {
		t.Fatalf("reader not released: %v", time.Since(start))
	}
}
