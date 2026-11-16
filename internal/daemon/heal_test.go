package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"vigil/internal/herdr"
	"vigil/internal/notify"
	"vigil/internal/resident"
	"vigil/internal/task"
)

type fakeSession struct{ running, started bool }

func (f *fakeSession) Running(context.Context) bool { return f.running }
func (f *fakeSession) StartServer() error           { f.started, f.running = true, true; return nil }

// vanishingHerdr never keeps an agent alive: every AgentGet says it is missing.
type vanishingHerdr struct {
	mu      sync.Mutex
	created int
	screen  string
	blocked bool
}

func (v *vanishingHerdr) AgentGet(context.Context, string) (herdr.Agent, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.blocked {
		return herdr.Agent{Status: herdr.StatusBlocked}, nil
	}
	return herdr.Agent{}, herdr.ErrNoAgent
}
func (v *vanishingHerdr) CreateWorkspace(context.Context, string, string) (string, error) {
	v.mu.Lock()
	v.created++
	v.mu.Unlock()
	return "w1:p1", nil
}
func (v *vanishingHerdr) StartAgent(context.Context, string, string, string, []string) error {
	return nil
}
func (v *vanishingHerdr) Prompt(context.Context, string, string) error { return nil }
func (v *vanishingHerdr) Read(context.Context, string, int) (string, error) {
	return v.screen, nil
}
func (v *vanishingHerdr) SendKeys(context.Context, string, ...string) error { return nil }

type countSender struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (c *countSender) Send(_ context.Context, m notify.Message, _ string) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	return nil
}

func healerFor(t *testing.T, h *vanishingHerdr) (*Healer, *fakeSession, *countSender) {
	t.Helper()
	s, e, _ := setup(t)
	tk := task.WithDefaults(task.Task{ID: "r", Name: "常驻", Enabled: true, Mode: task.ModeResident, Trigger: task.Trigger{Kind: task.TriggerManual}})
	if err := s.Put(tk); err != nil {
		t.Fatal(err)
	}
	sess := &fakeSession{}
	snd := &countSender{}
	healer := &Healer{Store: s, Engine: e, Session: sess, Notifier: snd, Logf: nolog,
		Resident: resident.Executor{H: h, Sleep: func(time.Duration) {}}}
	return healer, sess, snd
}

func TestHealerStartsSessionAndGivesUpAfterRepeatedRestarts(t *testing.T) {
	h := &vanishingHerdr{}
	healer, sess, snd := healerFor(t, h)
	defer healer.keepAwake(false)
	for range 5 {
		healer.Check(context.Background())
	}
	if !sess.started {
		t.Fatal("session not started")
	}
	if h.created != maxRestarts {
		t.Fatalf("agent recreated %d times, want %d", h.created, maxRestarts)
	}
	if len(snd.msgs) != 1 {
		t.Fatalf("want exactly one give-up notification, got %d", len(snd.msgs))
	}
	healer.Reset("r")
	healer.Check(context.Background())
	if h.created != maxRestarts+1 {
		t.Fatal("Reset should allow healing again")
	}
}

func TestHealerWarnsOnceAboutUnknownDialog(t *testing.T) {
	h := &vanishingHerdr{blocked: true, screen: "Do you want to proceed? (y/n)"}
	healer, _, snd := healerFor(t, h)
	defer healer.keepAwake(false)
	healer.Check(context.Background())
	healer.Check(context.Background())
	if len(snd.msgs) != 1 {
		t.Fatalf("want one warning, got %d", len(snd.msgs))
	}
}
