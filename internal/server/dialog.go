package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"vigil/internal/resident"
	"vigil/internal/runner"
	"vigil/internal/store"
	"vigil/internal/task"
)

// A resident agent stopped on a dialog Vigil does not answer itself (a permission prompt raised by the
// owner's own ask rules, say). The run that hit it asks the owner, with the dialog's one-off answers as
// options; picking one presses it in the agent's session. Rounds that came in while the agent was stuck
// never reached it, so they are run again once the dialog is answered.
const (
	dialogAnswerTimeout = 15 * time.Second
	dialogRunsScanned   = 100 // how far back the rounds stuck behind a dialog are looked for
)

// DialogAnswerer presses an answer in the dialog a task's agent is stopped on. shown is the dialog as the
// owner saw it; nothing is pressed when the screen shows something else.
type DialogAnswerer interface {
	AnswerDialog(ctx context.Context, t task.Task, shown, answer string) error
}

var errDialogChanged = &httpError{code: http.StatusConflict, msg: resident.ErrDialogChanged.Error()}

// answerDialog settles the dialog asked about in run asked and re-runs the rounds stuck behind it.
func (s *Server) answerDialog(id string, asked store.RunRecord, answer string) (map[string]any, error) {
	if s.Dialogs == nil {
		return nil, badRequest("没法从这里回答确认框：去 herdr 的 vigil 会话里处理")
	}
	t, err := s.Store.Get(id)
	if err != nil {
		return nil, err
	}
	runs, err := s.Store.Runs(id, dialogRunsScanned)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialogAnswerTimeout)
	defer cancel()
	if err := s.Dialogs.AnswerDialog(ctx, t, asked.Summary, answer); err != nil {
		if errors.Is(err, resident.ErrDialogChanged) {
			return nil, errDialogChanged
		}
		return nil, err
	}
	if _, err := s.Store.UpdateState(id, func(st store.State) store.State { st.AnsweredRun, st.AnsweredText = asked.ID, answer; return st }); err != nil {
		return nil, err
	}
	lost := stuckBehindDialog(runs)
	for _, input := range lost {
		s.Engine.Submit(runner.Request{TaskID: id, Trigger: runner.ByRetry, Input: dialogRerunNote + input})
	}
	if it, open, err := s.Inbox.OpenByRef(dialogTodoRef(id)); err == nil && open {
		s.closeDialogTodo(it, "已在会话里按下「"+answer+"」")
	}
	s.logf("dialog %s: pressed %q, re-running %d rounds", id, answer, len(lost))
	s.changed(id, "answered")
	return map[string]any{"id": id, "pressed": answer, "rerun": len(lost)}, nil
}

const dialogRerunNote = "（这一轮之前因为 agent 停在确认框上没送到，现在重跑。先确认上次有没有已经做过的动作，别重复发消息、别重复建待办。）\n"

// stuckBehindDialog is the inputs of the latest streak of dialog runs (newest first), except the oldest:
// that one was in progress when the dialog came up and carries on once it is answered; the later ones
// found the agent already stuck and were never sent. Background jobs are left to their parent.
func stuckBehindDialog(runs []store.RunRecord) []string {
	streak := 0
	for streak < len(runs) && runs[streak].Dialog {
		streak++
	}
	if streak < 2 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for i := streak - 2; i >= 0; i-- { // oldest first, so they run in the order they came
		r := runs[i]
		if r.Job != "" || r.Input == "" || seen[r.Input] {
			continue
		}
		seen[r.Input] = true
		out = append(out, r.Input)
	}
	return out
}
