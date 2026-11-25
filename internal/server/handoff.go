package server

import (
	"net/http"
	"regexp"

	"vigil/internal/inbox"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// A round that carries the owner's answer to a to-do (todoInput) can fail like any other. The task never
// got the answer, so nothing closes the to-do: without a mark the card said 「已交给它处理」 for good
// (11-25 td-60e205: handed over at 12:34, the first second of an account block).
const handoffErrMax = 300

var handoffTodo = regexp.MustCompile(todoInputLead + `(td-[0-9a-z]+)`)

// noteHandoff marks the to-do whose answer a failed round was carrying.
func (s *Server) noteHandoff(rec store.RunRecord) {
	if rec.Status != store.StatusFail && rec.Status != store.StatusTimeout {
		return
	}
	m := handoffTodo.FindStringSubmatch(rec.Input)
	if m == nil {
		return
	}
	it, err := s.Inbox.HandoffFailed(m[1], clipRunes(oneLine(firstNonEmpty(rec.Error, rec.Summary, rec.Status)), handoffErrMax))
	if err != nil {
		return // answered again, or closed meanwhile: nothing is stuck
	}
	s.Engine.Hub().Publish(runner.Event{Type: "todo.updated", TaskID: it.TaskID, Text: it.Title})
}

func (s *Server) todoResend(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.ResendTodo(r.PathValue("id"))
}

// ResendTodo hands an answered to-do's answer to its task again, the way it first went out.
func (s *Server) ResendTodo(id string) (inbox.Item, error) {
	it, err := s.Inbox.Resend(id)
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	mode := TodoInstruct
	if it.Decision == inbox.DecisionAsIs || it.Decision == inbox.DecisionEdited {
		mode = TodoSend
	}
	s.Engine.Submit(runner.Request{TaskID: it.TaskID, Trigger: runner.ByAnswer, Input: todoInput(it, mode, s.vigilBin())})
	s.Engine.Hub().Publish(runner.Event{Type: "todo.answered", TaskID: it.TaskID, Text: it.Title})
	return it, nil
}
