package server

import (
	"testing"
	"time"

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

// Putting a failing check away lasts its full day even when Vigil restarts (a reinstall) in between.
func TestAPutAwayCheckStaysAwayAcrossARestart(t *testing.T) {
	s, _ := newServer(t)
	now := time.Date(2026, 11, 16, 17, 0, 0, 0, time.Local)
	s.Now = func() time.Time { return now }
	c := health.Check{Name: "gh", Hint: "跑 gh auth login"}
	_ = s.RaiseCheck(c, "deploy: ❌")
	open, _ := s.Inbox.List(false)
	if _, err := s.AnswerTodo(open[0].ID, TodoDismiss, ""); err != nil {
		t.Fatal(err)
	}

	restarted := &Server{Store: s.Store, Inbox: s.Inbox, Engine: s.Engine, Now: s.Now}
	_ = restarted.RaiseCheck(c, "still ❌")

	if open, _ := s.Inbox.List(false); len(open) != 0 {
		t.Fatalf("raised again after a restart: %+v", open)
	}
	restarted.Now = func() time.Time { return now.Add(checkMute + time.Minute) }
	_ = restarted.RaiseCheck(c, "still ❌")
	if open, _ := s.Inbox.List(false); len(open) != 1 {
		t.Fatal("not raised again after the day was over")
	}
}

// The card says what failed: the check's own report heads the text, before the generic hint.
func TestACheckCardShowsWhatFailed(t *testing.T) {
	s, _ := newServer(t)
	c := health.Check{Name: "devcli", Hint: "在终端跑上面最后一行的命令"}

	_ = s.RaiseCheck(c, "gateway: 需要重新登录\ndb-console: 还剩 20m 过期\n\n在终端跑：devcli login --only gateway,db-console\n（exit status 1）")

	open, _ := s.Inbox.List(false)
	want := "gateway: 需要重新登录\ndb-console: 还剩 20m 过期\n在终端跑：devcli login --only gateway,db-console\n\n在终端跑上面最后一行的命令"
	if len(open) != 1 || open[0].Detail != want {
		t.Fatalf("detail = %q", open[0].Detail)
	}
}
