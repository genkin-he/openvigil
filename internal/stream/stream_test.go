package stream

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSpoolAppendPendingAck(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(100, 0)
	a, _ := s.Append("one", at)
	b, _ := s.Append("two", at) // same timestamp still gets a larger seq
	_, _ = s.Append("three", at)
	if b.Seq <= a.Seq {
		t.Fatalf("seq not increasing: %d %d", a.Seq, b.Seq)
	}
	got, _ := s.Pending(2)
	if len(got) != 2 || got[0].Line != "one" {
		t.Fatalf("Pending = %+v", got)
	}
	if err := s.Ack(b.Seq); err != nil {
		t.Fatal(err)
	}
	reopened, _ := OpenSpool(dir)
	rest, _ := reopened.Pending(0)
	if len(rest) != 1 || rest[0].Line != "three" {
		t.Fatalf("after ack = %+v", rest)
	}
	c, _ := reopened.Append("four", at)
	if c.Seq <= rest[0].Seq {
		t.Fatal("seq regressed after reopen")
	}
}

func TestSourceRestartsWithBackoff(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	var streaks []int
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	src := Source{
		Command: "echo hello; exit 1",
		OnLine:  func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() },
		OnExit: func(_ error, streak int) {
			mu.Lock()
			streaks = append(streaks, streak)
			if len(streaks) == 3 {
				cancel()
			}
			mu.Unlock()
		},
		Sleep: func(_ context.Context, d time.Duration) { waits = append(waits, d) },
	}
	done := make(chan struct{})
	go func() { src.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("source did not stop")
	}
	if len(lines) < 3 || lines[0] != "hello" {
		t.Fatalf("lines = %v", lines)
	}
	if len(waits) < 2 || waits[1] != 2*time.Second || streaks[2] != 3 {
		t.Fatalf("waits=%v streaks=%v", waits, streaks)
	}
}
