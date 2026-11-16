package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vigil/internal/exec"
	"vigil/internal/store"
	"vigil/internal/task"
)

func waitIdle(t *testing.T, e *Engine, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.Busy(id) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still busy", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDrainHoldsNewRunsAndResumes(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(task.WithDefaults(manual("cms"))); err != nil {
		t.Fatal(err)
	}
	g := &jobGate{release: make(chan struct{}), started: make(chan string, 1)}
	e := New(context.Background(), Config{Store: s, Headless: g, AfterFunc: func(time.Duration, func()) {}})

	e.Drain(true, time.Minute)
	e.Submit(Request{TaskID: "cms", Trigger: ByManual, Input: "第一件"})
	time.Sleep(50 * time.Millisecond)
	if runs, _ := s.Runs("cms", 0); len(runs) != 0 {
		t.Fatalf("a run started while draining: %+v", runs)
	}
	if p := e.Pending(); len(p) != 1 || p[0].Input != "第一件" {
		t.Fatalf("pending = %+v", p)
	}

	e.Drain(false, 0)
	waitIdle(t, e, "cms")
	e.Wait()
	if runs, _ := s.Runs("cms", 0); len(runs) != 1 || runs[0].Status != store.StatusOK {
		t.Fatalf("queued run did not run after the drain ended: %+v", runs)
	}
}

func TestDrainExpiresByItself(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(task.WithDefaults(manual("cms"))); err != nil {
		t.Fatal(err)
	}
	var expire func()
	e := New(context.Background(), Config{Store: s, Headless: &jobGate{}, AfterFunc: func(_ time.Duration, f func()) { expire = f }})
	e.Drain(true, time.Minute)
	e.Submit(Request{TaskID: "cms", Trigger: ByManual})
	if expire == nil || !e.Draining() {
		t.Fatal("drain did not arm its expiry")
	}
	expire()
	if e.Draining() {
		t.Fatal("drain did not expire")
	}
	waitIdle(t, e, "cms")
	e.Wait()
	if runs, _ := s.Runs("cms", 0); len(runs) != 1 {
		t.Fatalf("runs after expiry = %+v", runs)
	}
}

func TestQueueSurvivesRestart(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "queue.json")
	e := New(context.Background(), Config{Store: s, Headless: &jobGate{}})
	e.Drain(true, 0)
	e.Submit(Request{TaskID: "cms", Trigger: ByAnswer, Input: "发这条"})
	e.Submit(Request{TaskID: "slack", Trigger: ByJob, Job: "j-1", JobTitle: "排查", Input: "查订单"})
	if err := e.SaveQueue(path); err != nil {
		t.Fatal(err)
	}
	reqs, err := LoadQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 || reqs[0].Input != "发这条" || reqs[1].Job != "j-1" {
		t.Fatalf("loaded = %+v", reqs)
	}
	if err := RemoveQueue(path); err != nil {
		t.Fatal(err)
	}
	if again, _ := LoadQueue(path); len(again) != 0 {
		t.Fatal("queue file must be gone after RemoveQueue")
	}
}

// concurrencyProbe fails the test if two runs of the task overlap.
type concurrencyProbe struct {
	mu      sync.Mutex
	running int
	overlap bool
}

func (p *concurrencyProbe) Run(_ context.Context, _ exec.Request) exec.Outcome {
	p.mu.Lock()
	p.running++
	if p.running > 1 {
		p.overlap = true
	}
	p.mu.Unlock()
	time.Sleep(time.Millisecond)
	p.mu.Lock()
	p.running--
	p.mu.Unlock()
	return exec.Outcome{FinalText: okBlock}
}

func TestDrainTogglingNeverRunsALaneTwice(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(task.WithDefaults(manual("cms"))); err != nil {
		t.Fatal(err)
	}
	p := &concurrencyProbe{}
	e := New(context.Background(), Config{Store: s, Headless: p, MaxConcurrent: 4, AfterFunc: func(time.Duration, func()) {}})
	for i := 0; i < 300; i++ {
		e.Submit(Request{TaskID: "cms", Trigger: ByManual, Input: fmt.Sprint(i)})
		e.Drain(i%2 == 0, time.Minute)
	}
	e.Drain(false, 0)
	e.Wait()
	if p.overlap {
		t.Fatal("two runs of one task overlapped")
	}
}
