package server

import (
	"testing"

	"vigil/internal/health"
	"vigil/internal/inbox"
)

func TestCheckTodoLifecycle(t *testing.T) {
	s, _ := newServer(t)
	c := health.Check{Name: "gh", Hint: "跑 gh auth login"}
	rechecked := 0
	s.RecheckNow = func() { rechecked++ }

	if err := s.RaiseCheck(c, "deploy: ❌"); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseCheck(c, "deploy-us: ❌"); err != nil {
		t.Fatal(err)
	}
	items, _ := s.Inbox.List(false)
	if len(items) != 1 || items[0].TaskID != inbox.SystemTask || items[0].Ref != "check:gh" {
		t.Fatalf("items = %+v", items)
	}

	// Answering a system to-do only asks for a recheck; it stays open until the check passes.
	if it, err := s.AnswerTodo(items[0].ID, TodoInstruct, "登好了"); err != nil || it.Status != inbox.StatusOpen || rechecked != 1 {
		t.Fatalf("answer: %+v %v rechecked=%d", it, err, rechecked)
	}
	if err := s.ResolveCheck(c); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.Inbox.List(false); len(open) != 0 {
		t.Fatalf("not resolved: %+v", open)
	}

	// Put away while failing: no new to-do for a day.
	_ = s.RaiseCheck(c, "again")
	open, _ := s.Inbox.List(false)
	if _, err := s.AnswerTodo(open[0].ID, TodoDismiss, ""); err != nil {
		t.Fatal(err)
	}
	_ = s.RaiseCheck(c, "still")
	if open, _ := s.Inbox.List(false); len(open) != 0 {
		t.Fatalf("muted check raised again: %+v", open)
	}
}

// A recheck that still fails refreshes the open to-do; the console has to hear about it to stop showing
// "rechecking" and say it is still broken.
func TestAStillFailingCheckTellsTheConsole(t *testing.T) {
	s, _ := newServer(t)
	c := health.Check{Name: "gh"}
	if err := s.RaiseCheck(c, "first"); err != nil {
		t.Fatal(err)
	}
	events, stop := s.Engine.Hub().Subscribe()
	defer stop()
	if err := s.RaiseCheck(c, "still broken"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Type != "todo.updated" {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("no event for a refreshed check to-do")
	}
}
