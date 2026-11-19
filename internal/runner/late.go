package runner

import (
	"fmt"

	"vigil/internal/result"
	"vigil/internal/store"
	"vigil/internal/task"
)

// lateScan is how far back in each resident task's runs a late report looks for its round.
const lateScan = 100

// lateReport keeps a report for a resident round that was closed as stuck on a dialog: once the owner
// answers it, the agent finishes the round and reports, and what it did and learned is not thrown away.
// Any other run that is no longer waiting gets ErrUnknownRun.
func (e *Engine) lateReport(runID string, r result.Result) error {
	t, rec, ok := e.findDialogRun(runID)
	if !ok {
		return ErrUnknownRun
	}
	s := e.cfg.Store
	if len(r.Memory) > 0 {
		if err := s.AppendMemory(t.ID, r.Memory, e.cfg.Now()); err != nil {
			return fmt.Errorf("memory %s: %w", t.ID, err)
		}
	}
	rec.Status, rec.Summary, rec.Structured, rec.Dialog, rec.Made = r.Status, r.Summary, true, false, r.Made
	rec.Question, rec.Options, rec.Recommendation = r.Question, r.Options, r.Recommendation
	if err := s.AppendRun(rec); err != nil {
		return fmt.Errorf("record late report: %w", err)
	}
	e.cfg.Logf("%s: late report for %s, after its dialog", t.ID, runID)
	e.cfg.Hub.Publish(Event{Type: "run.finished", TaskID: t.ID, RunID: rec.ID, Status: rec.Status, Text: rec.Summary})
	e.notify(t, rec, 0)
	return nil
}

// findDialogRun finds runID among the resident tasks' recent runs, if it is a round still closed as
// waiting on a dialog.
func (e *Engine) findDialogRun(runID string) (task.Task, store.RunRecord, bool) {
	tasks, _ := e.cfg.Store.List()
	for _, t := range tasks {
		if t.Mode != task.ModeResident {
			continue
		}
		runs, err := e.cfg.Store.Runs(t.ID, lateScan)
		if err != nil {
			continue
		}
		for _, rec := range runs {
			if rec.ID == runID {
				return t, rec, rec.Dialog && rec.Status == store.StatusNeedsYou
			}
		}
	}
	return task.Task{}, store.RunRecord{}, false
}
