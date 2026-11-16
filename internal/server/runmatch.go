package server

import (
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// legacyRunWindow is how far before a to-do's creation a run may have started and still be taken as
// its source, when nothing else ties them together.
const legacyRunWindow = 10 * time.Minute

// liveRunOf returns the run on a task's main lane that is in progress (background jobs excluded).
func liveRunOf(live []runner.LiveRun, taskID string) string {
	for _, r := range live {
		if r.TaskID == taskID && r.Job == "" {
			return r.RunID
		}
	}
	return ""
}

// runOf finds the run that raised a to-do: by its recorded run id; for older items, the latest run
// before it whose input carries the Slack ts in the item's ref (channel:ts[:thread]), else the latest
// run that started within legacyRunWindow before it.
func (s *Server) runOf(it inbox.Item) (store.RunRecord, bool) {
	runs, err := s.Store.Runs(it.TaskID, 0)
	if err != nil {
		return store.RunRecord{}, false
	}
	if it.RunID != "" {
		for _, r := range runs {
			if r.ID == it.RunID && r.Status != store.StatusRunning {
				return r, true
			}
		}
		return store.RunRecord{}, false
	}
	ts := refTS(it.Ref)
	var byTime store.RunRecord
	found := false
	for _, r := range runs { // newest first
		if r.Status == store.StatusRunning || r.StartedAt.After(it.Created) {
			continue
		}
		if ts != "" && strings.Contains(r.Input, ts) {
			return r, true
		}
		if !found && it.Created.Sub(r.StartedAt) <= legacyRunWindow {
			byTime, found = r, true
		}
	}
	return byTime, found
}

// refTS returns the message ts of a channel:ts[:thread] ref, or "" when the ref is not one.
func refTS(ref string) string {
	parts := strings.Split(ref, ":")
	if len(parts) < 2 || !strings.Contains(parts[1], ".") {
		return ""
	}
	return parts[1]
}
