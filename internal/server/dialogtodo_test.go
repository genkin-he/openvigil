package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/resident"
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

// stuckDetourRun is a detour that found the agent stuck on the dialog summary.
func stuckDetourRun(t *testing.T, s *Server, id, summary string, at int64) {
	t.Helper()
	if err := s.Store.AppendRun(store.RunRecord{ID: id, TaskID: "slack", Trigger: runner.ByStream, Status: store.StatusOK,
		Detour: true, Summary: "（常驻会话停在确认框上，这一轮改在临时会话里跑了）\n回了一条", Input: "x",
		StuckOn:   &store.StuckDialog{Summary: summary, Question: "agent 在等你回答「Do you want to proceed?」", Options: []string{"Yes", "No"}},
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

// 11-23: the owner pressed Yes on a prod-query dialog, the agent ran it and then stopped on a second
// prod-query dialog in the same round. The later rounds detoured, but no to-do asked about the second one.
func TestASecondDialogInTheSameRoundIsAskedToo(t *testing.T) {
	s, f := slackTask(t)
	s.Now = func() time.Time { return time.Unix(150, 0) }
	dialogRun(t, s, "r1", "小林：在吗", 100)
	s.syncDialogTodo("slack")
	first := openDialogTodos(t, s)[0]
	if _, err := s.AnswerTodo(first.ID, TodoSend, "Yes"); err != nil {
		t.Fatal(err)
	}
	stuckDetourRun(t, s, "d0", "Bash command：回报本轮结果", 140) // started before the answer: the old dialog
	s.syncDialogTodo("slack")
	if n := len(openDialogTodos(t, s)); n != 0 {
		t.Fatalf("%d todos reopened for the dialog already answered", n)
	}

	stuckDetourRun(t, s, "d1", "Bash command：第二条生产查询", 200)
	stuckDetourRun(t, s, "d2", "Bash command：第二条生产查询", 300)
	s.syncDialogTodo("slack")
	s.syncDialogTodo("slack")
	todos := openDialogTodos(t, s)
	if len(todos) != 1 || todos[0].Detail != "Bash command：第二条生产查询" || len(todos[0].Choices) != 2 {
		t.Fatalf("todos = %+v", todos)
	}
	if _, err := s.AnswerTodo(todos[0].ID, TodoSend, "Yes"); err != nil {
		t.Fatal(err)
	}
	if len(f.pressed) != 2 || f.pressed[1] != "slack|Bash command：第二条生产查询|Yes" {
		t.Fatalf("pressed = %q", f.pressed)
	}
	s.syncDialogTodo("slack")
	if n := len(openDialogTodos(t, s)); n != 0 {
		t.Fatalf("%d todos left after answering the second dialog", n)
	}
}

// The same second dialog can be answered from the task card.
func TestASecondDialogCanBeAnsweredFromTheTask(t *testing.T) {
	s, f := slackTask(t)
	s.Now = func() time.Time { return time.Unix(150, 0) }
	dialogRun(t, s, "r1", "小林：在吗", 100)
	if _, err := s.Answer("slack", "Yes"); err != nil {
		t.Fatal(err)
	}
	stuckDetourRun(t, s, "d1", "Bash command：第二条生产查询", 200)
	if _, err := s.Answer("slack", "Yes"); err != nil {
		t.Fatal(err)
	}
	if len(f.pressed) != 2 || f.pressed[1] != "slack|Bash command：第二条生产查询|Yes" {
		t.Fatalf("pressed = %q", f.pressed)
	}
}

// A detour stuck on the dialog the open to-do already asks about does not replace that to-do.
func TestADetourOnTheSameDialogKeepsTheTodo(t *testing.T) {
	s, _ := slackTask(t)
	dialogRun(t, s, "r1", "小林：在吗", 100)
	s.syncDialogTodo("slack")
	stuckDetourRun(t, s, "d1", "Bash command：回报本轮结果", 200)
	s.syncDialogTodo("slack")
	todos := openDialogTodos(t, s)
	if len(todos) != 1 || todos[0].RunID != "r1" {
		t.Fatalf("todos = %+v", todos)
	}
}

// A press that finds the dialog gone must not bring the same to-do back on the next sweep: the agent left
// that dialog, and the detours that saw it are history.
func TestADialogFoundGoneIsNotAskedAgain(t *testing.T) {
	s, f := slackTask(t)
	s.Now = func() time.Time { return time.Unix(150, 0) }
	stuckDetourRun(t, s, "d1", "Bash command：python3 -c …", 100)
	s.syncDialogTodo("slack")
	it := openDialogTodos(t, s)[0]
	f.err = resident.ErrDialogChanged
	if _, err := s.AnswerTodo(it.ID, TodoSend, "Yes"); err == nil {
		t.Fatal("a press on a vanished dialog reported success")
	}
	s.syncDialogTodo("slack")
	if todos := openDialogTodos(t, s); len(todos) != 0 {
		t.Fatalf("the vanished dialog was asked again: %+v", todos)
	}
	stuckDetourRun(t, s, "d2", "Bash command：deployctl apply …", 200) // the agent's next dialog is still asked
	s.syncDialogTodo("slack")
	if todos := openDialogTodos(t, s); len(todos) != 1 || todos[0].RunID != "d2" {
		t.Fatalf("the next dialog was not asked: %+v", todos)
	}
}

// A raised to-do never offers an answer that changes settings for later prompts.
func TestADialogTodoLeavesOutLastingAllows(t *testing.T) {
	s, _ := slackTask(t)
	if err := s.Store.AppendRun(store.RunRecord{ID: "d1", TaskID: "slack", Trigger: runner.ByStream, Status: store.StatusOK,
		Detour: true, Summary: runner.DetourNote + "x", Input: "x",
		StuckOn: &store.StuckDialog{Summary: "Bash command", Question: "agent 在等你回答「Do you want to proceed?」",
			Options: []string{"Yes", "Yes, and don’t ask again for: python3 -c ' *", "No"}},
		StartedAt: time.Unix(100, 0), EndedAt: time.Unix(101, 0)}); err != nil {
		t.Fatal(err)
	}
	s.syncDialogTodo("slack")
	it := openDialogTodos(t, s)[0]
	if got := choiceLabels(it); strings.Join(got, "|") != "Yes|No" {
		t.Fatalf("choices = %q", got)
	}
}
