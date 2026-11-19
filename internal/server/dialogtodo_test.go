package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// detourRun is a round that ran in a fresh session because the resident agent was stuck on a dialog.
func detourRun(t *testing.T, s *Server, id string, at int64) {
	t.Helper()
	if err := s.Store.AppendRun(store.RunRecord{ID: id, TaskID: "slack", Trigger: runner.ByStream, Status: store.StatusOK,
		Detour: true, Summary: "（常驻会话停在确认框上，这一轮改在临时会话里跑了）\n回了一条", Input: "x",
		StartedAt: time.Unix(at, 0), EndedAt: time.Unix(at+1, 0)}); err != nil {
		t.Fatal(err)
	}
}

func okRun(t *testing.T, s *Server, id string, at int64) {
	t.Helper()
	if err := s.Store.AppendRun(store.RunRecord{ID: id, TaskID: "slack", Trigger: runner.ByStream, Status: store.StatusOK,
		Summary: "照常", Input: "y", StartedAt: time.Unix(at, 0), EndedAt: time.Unix(at+1, 0)}); err != nil {
		t.Fatal(err)
	}
}

func slackTask(t *testing.T) (*Server, *fakeDialogs) {
	t.Helper()
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	f := &fakeDialogs{}
	s.Dialogs = f
	return s, f
}

func openDialogTodos(t *testing.T, s *Server) []inbox.Item {
	t.Helper()
	items, err := s.Inbox.List(false)
	if err != nil {
		t.Fatal(err)
	}
	var out []inbox.Item
	for _, it := range items {
		if isDialogTodo(it) && it.Status == inbox.StatusOpen {
			out = append(out, it)
		}
	}
	return out
}

func TestADialogStaysAskedWhileLaterRoundsDetour(t *testing.T) {
	s, _ := slackTask(t)
	dialogRun(t, s, "r1", "小林：在吗", 100)
	for i := range 12 { // more detours than the answer path used to look back over
		detourRun(t, s, fmt.Sprintf("d%02d", i), int64(200+i*10))
	}
	tk, err := s.Store.Get("slack")
	if err != nil {
		t.Fatal(err)
	}
	v := s.view(tk, nil, time.Unix(1000, 0))
	if v.NeedsYou == nil || v.NeedsYou.ID != "r1" {
		t.Fatalf("needs you = %+v, want the dialog round r1", v.NeedsYou)
	}
	if _, err := s.Answer("slack", "Yes"); err != nil {
		t.Fatal(err)
	}
}

func TestAnsweringAfterDetoursPressesTheDialog(t *testing.T) {
	s, f := slackTask(t)
	dialogRun(t, s, "r1", "小林：在吗", 100)
	for i := range 12 {
		detourRun(t, s, fmt.Sprintf("d%02d", i), int64(200+i*10))
	}
	if _, err := s.Answer("slack", "Yes"); err != nil {
		t.Fatal(err)
	}
	if len(f.pressed) != 1 || !strings.HasSuffix(f.pressed[0], "|Yes") {
		t.Fatalf("pressed = %q", f.pressed)
	}
}

func TestAnOpenDialogBecomesOneTodoAnsweredByItsButtons(t *testing.T) {
	s, f := slackTask(t)
	dialogRun(t, s, "r1", "小林：在吗", 100)
	detourRun(t, s, "d1", 200)
	s.syncDialogTodo("slack")
	s.syncDialogTodo("slack") // a second sweep must not add another
	todos := openDialogTodos(t, s)
	if len(todos) != 1 {
		t.Fatalf("todos = %+v", todos)
	}
	it := todos[0]
	if it.RunID != "r1" || it.Kind != dialogTodoKind || len(it.Choices) != 2 || it.Choices[0].Label != "Yes" ||
		!strings.Contains(it.Detail, "回报本轮结果") {
		t.Fatalf("todo = %+v", it)
	}
	if _, err := s.AnswerTodo(it.ID, TodoSend, "Yes"); err != nil {
		t.Fatal(err)
	}
	if len(f.pressed) != 1 || !strings.HasSuffix(f.pressed[0], "|Yes") {
		t.Fatalf("pressed = %q", f.pressed)
	}
	got, _ := s.Inbox.Get(it.ID)
	if got.Status != inbox.StatusDone {
		t.Fatalf("todo still %s after the dialog was pressed", got.Status)
	}
	s.syncDialogTodo("slack") // answered: no new todo for the same dialog
	if n := len(openDialogTodos(t, s)); n != 0 {
		t.Fatalf("%d dialog todos reopened after answering", n)
	}
}

func TestADialogTodoRefusesAnAnswerThatIsNotOneOfItsButtons(t *testing.T) {
	s, f := slackTask(t)
	dialogRun(t, s, "r1", "小林：在吗", 100)
	s.syncDialogTodo("slack")
	it := openDialogTodos(t, s)[0]
	if _, err := s.AnswerTodo(it.ID, TodoSend, "随便"); err == nil {
		t.Fatal("a free-text answer pressed nothing but was accepted")
	}
	if len(f.pressed) != 0 {
		t.Fatalf("pressed = %q", f.pressed)
	}
}

func TestADialogTodoClosesOnceTheAgentMovedOn(t *testing.T) {
	s, _ := slackTask(t)
	dialogRun(t, s, "r1", "小林：在吗", 100)
	s.syncDialogTodo("slack")
	okRun(t, s, "r2", 200) // the owner pressed it in herdr; the agent's own next round went through
	s.syncDialogTodo("slack")
	if n := len(openDialogTodos(t, s)); n != 0 {
		t.Fatalf("%d dialog todos left open after the agent moved on", n)
	}
}

func TestAnsweringFromTheTaskClosesTheDialogTodo(t *testing.T) {
	s, _ := slackTask(t)
	dialogRun(t, s, "r1", "小林：在吗", 100)
	s.syncDialogTodo("slack")
	if _, err := s.Answer("slack", "Yes"); err != nil {
		t.Fatal(err)
	}
	if n := len(openDialogTodos(t, s)); n != 0 {
		t.Fatalf("%d dialog todos left open after answering from the task", n)
	}
}
