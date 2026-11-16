package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"vigil/internal/task"
)

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fastSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(30 * time.Millisecond):
	}
}

func TestProducerRestartsSurvivesVigilAndIsAdopted(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "p.pid")
	spec := task.Producer{Command: "exec sleep 37", PIDFile: pidfile, Match: "sleep 37"}
	p := Producer{Spec: spec, Logf: nolog, Sleep: fastSleep}

	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	waitFor(t, "first start", func() bool { _, ok := p.alive(); return ok })
	first := readPID(t, pidfile)

	_ = syscall.Kill(first, syscall.SIGKILL)
	waitFor(t, "restart after kill", func() bool { pid, ok := p.alive(); return ok && pid != first })
	second := readPID(t, pidfile)

	cancel() // Vigil stops: the producer must keep running
	time.Sleep(100 * time.Millisecond)
	if _, ok := p.alive(); !ok {
		t.Fatal("producer died with Vigil")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go p.Run(ctx2) // a new Vigil adopts it instead of starting another
	time.Sleep(150 * time.Millisecond)
	if readPID(t, pidfile) != second {
		t.Fatal("running producer was replaced instead of adopted")
	}

	p.Stop()
	if syscall.Kill(second, 0) == nil {
		t.Fatal("Stop left the producer running")
	}
}

func TestProducerIgnoresReusedPID(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "p.pid")
	_ = os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o600) // alive, but not our producer
	p := Producer{Spec: task.Producer{Command: "true", PIDFile: pidfile, Match: "no-such-command-line"}}
	if _, ok := p.alive(); ok {
		t.Fatal("a reused pid must not count as the producer")
	}
}
