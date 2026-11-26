package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/resident"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// A resident agent stuck on a dialog only the owner may answer (a permission prompt raised by the
// owner's own ask rules) used to surface on the task card alone, and only while it was the task's
// latest round. Later rounds detour into fresh sessions and succeed, so the card dropped the question
// although the agent was still stuck. The open dialog is now found behind those detours, and it becomes
// a to-do whose buttons are the dialog's answers: it reaches the to-do list and the owner's self-DM like
// any other decision, and closes itself once the dialog is answered or the agent has moved on.

const (
	dialogTodoKind      = "确认框"
	dialogTodoRefPrefix = "dialog:"
	dialogSweepEvery    = 2 * time.Minute
	dialogTodoChoiceMax = inbox.MaxChoiceLabel
)

func dialogTodoRef(taskID string) string { return dialogTodoRefPrefix + taskID }

func isDialogTodo(it inbox.Item) bool { return strings.HasPrefix(it.Ref, dialogTodoRefPrefix) }

func isDetour(r store.RunRecord) bool {
	return r.Detour || strings.HasPrefix(r.Summary, runner.DetourNote)
}

// openDialog is the dialog the task's resident agent is still stuck on, runs newest first. It is the newest
// round that is not a detour or still in progress, when that round stopped on a dialog nobody has answered
// from Vigil, unless a later detour found the agent on another dialog: after one dialog is answered the
// agent carries on with the same round and may stop on the next one, which only the detours see. Detours
// that started before the last answer saw the dialog that answer settled.
func openDialog(runs []store.RunRecord, st store.State) (store.RunRecord, bool) {
	var stuck *store.RunRecord
	sawStuck := false // only the newest detour that saw a dialog tells what the agent is on now
	for i, r := range runs {
		if r.Status == store.StatusRunning || r.Status == store.StatusSkipped || r.Job != "" {
			continue
		}
		if isDetour(r) {
			if !sawStuck && r.StuckOn != nil {
				sawStuck = true
				if r.StartedAt.After(st.AnsweredAt) && st.AnsweredRun != r.ID {
					stuck = &runs[i]
				}
			}
			continue
		}
		answered := st.AnsweredRun == r.ID || r.StartedAt.Before(st.AnsweredAt)
		if r.Dialog && r.Status == store.StatusNeedsYou && !answered &&
			(stuck == nil || stuck.StuckOn.Summary == r.Summary) {
			return r, true
		}
		break
	}
	if stuck != nil {
		return dialogRecord(*stuck), true
	}
	return store.RunRecord{}, false
}

// dialogRecord is a run as the dialog it asks about: a detour stands for the dialog it found the agent on.
func dialogRecord(r store.RunRecord) store.RunRecord {
	if r.StuckOn == nil {
		return r
	}
	return store.RunRecord{ID: r.ID, TaskID: r.TaskID, Trigger: r.Trigger, Status: store.StatusNeedsYou, Dialog: true,
		Summary: r.StuckOn.Summary, Question: r.StuckOn.Question, Options: r.StuckOn.Options, StartedAt: r.StartedAt}
}

// taskOpenDialog loads what openDialog needs for one task.
func (s *Server) taskOpenDialog(taskID string) (store.RunRecord, bool) {
	runs, err := s.Store.Runs(taskID, dialogRunsScanned)
	if err != nil {
		return store.RunRecord{}, false
	}
	st, _ := s.Store.State(taskID)
	return openDialog(runs, st)
}

// syncDialogTodo makes the task's to-do list match its agent: one open to-do while a dialog is waiting,
// none once it is answered or gone.
func (s *Server) syncDialogTodo(taskID string) {
	if s.Inbox == nil {
		return
	}
	ref := dialogTodoRef(taskID)
	rec, waiting := s.taskOpenDialog(taskID)
	existing, open, err := s.Inbox.OpenByRef(ref)
	if err != nil {
		s.logf("dialog todo %s: %v", taskID, err)
		return
	}
	switch {
	case !waiting && open:
		s.closeDialogTodo(existing, "确认框已经不在了（已处理或 agent 已继续）")
	case waiting && open && existing.RunID == rec.ID:
		// already asked about this dialog
	case waiting:
		if open { // a different dialog than the one asked about: replace it
			s.closeDialogTodo(existing, "agent 换了一个确认框，见新的待办")
		}
		s.raiseDialogTodo(taskID, rec)
	}
}

func (s *Server) raiseDialogTodo(taskID string, rec store.RunRecord) {
	name := taskID
	if t, err := s.Store.Get(taskID); err == nil && t.Name != "" {
		name = t.Name
	}
	var choices []inbox.Choice
	for _, o := range (resident.Prompt{Options: rec.Options}).Answers() { // lasting allows stay in herdr
		c := inbox.Choice{Label: clipRunes(o, dialogTodoChoiceMax)}
		if c.Label != o {
			c.Note = clipRunes(o, inbox.MaxChoiceNote)
		}
		choices = append(choices, c)
	}
	if len(choices) < inbox.MinChoices || len(choices) > inbox.MaxChoices {
		choices = nil // the to-do still says where to answer; the task card keeps the full list
	}
	it := inbox.Item{
		TaskID: taskID,
		Title:  name + "：" + rec.Question,
		Detail: rec.Summary,
		Analysis: "常驻会话停在这个确认框上，等你决定。之后来的轮次在临时会话里照常处理；" +
			"按下后常驻会话继续原来那一轮。点下面的按钮就是在会话里按对应选项。",
		Ref:     dialogTodoRef(taskID),
		RunID:   rec.ID,
		Kind:    dialogTodoKind,
		Choices: choices,
	}
	added, _, err := s.Inbox.Add(it)
	if err != nil {
		s.logf("dialog todo %s: %v", taskID, err)
		return
	}
	s.todoChanged(added, "todo.added")
}

func (s *Server) closeDialogTodo(it inbox.Item, outcome string) {
	closed, err := s.Inbox.CloseIfOpen(it.ID, outcome)
	if err != nil {
		s.logf("dialog todo %s: %v", it.TaskID, err)
		return
	}
	s.todoChanged(closed, "todo.closed")
}

// settleDialogTodo answers a dialog to-do: one of its buttons presses that option in the agent's
// session; dismissing it only closes the to-do, the agent stays stuck until someone answers.
func (s *Server) settleDialogTodo(it inbox.Item, mode, text string) (inbox.Item, error) {
	if mode == TodoDismiss {
		closed, err := s.Inbox.Close(it.ID, firstNonEmpty(strings.TrimSpace(text), "先不处理，确认框还在会话里"))
		if err != nil {
			return inbox.Item{}, todoErr(err)
		}
		s.todoChanged(closed, "todo.closed")
		return closed, nil
	}
	answer, ok := dialogChoice(it, strings.TrimSpace(text))
	if !ok {
		return inbox.Item{}, badRequest("确认框只能按它自己的选项：%s", strings.Join(choiceLabels(it), "、"))
	}
	rec, found := s.runRecord(it.TaskID, it.RunID)
	if !found {
		return inbox.Item{}, badRequest("找不到提出这个确认框的那一轮 %s", it.RunID)
	}
	rec = dialogRecord(rec)
	if _, err := s.answerDialog(it.TaskID, rec, answer); err != nil {
		if errors.Is(err, errDialogChanged) {
			s.closeDialogTodo(it, "确认框已经不在了，没有按任何选项")
		}
		return inbox.Item{}, err
	}
	got, err := s.Inbox.Get(it.ID)
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	return got, nil
}

// dialogChoice maps a picked button (its label, or the full option text) to the option to press.
func dialogChoice(it inbox.Item, text string) (string, bool) {
	for _, c := range it.Choices {
		full := firstNonEmpty(c.Note, c.Label)
		if text == c.Label || text == full {
			return full, true
		}
	}
	return "", false
}

func choiceLabels(it inbox.Item) []string {
	out := make([]string, len(it.Choices))
	for i, c := range it.Choices {
		out[i] = c.Label
	}
	return out
}

// DialogWatchLoop keeps dialog to-dos in step with the agents: at once when a round finishes, and on a
// timer, so a missed event or an answer pressed directly in herdr is caught too.
func (s *Server) DialogWatchLoop(ctx context.Context) {
	events, stop := s.Engine.Hub().Subscribe()
	defer stop()
	tick := time.NewTicker(dialogSweepEvery)
	defer tick.Stop()
	s.sweepDialogTodos()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Type == "run.finished" {
				s.syncDialogTodo(ev.TaskID)
			}
		case <-tick.C:
			s.sweepDialogTodos()
		}
	}
}

func (s *Server) sweepDialogTodos() {
	tasks, errs := s.Store.List() // a broken task file must not stop the others from being checked
	for _, err := range errs {
		s.logf("dialog todos: %v", err)
	}
	for _, t := range tasks {
		s.syncDialogTodo(t.ID)
	}
}
