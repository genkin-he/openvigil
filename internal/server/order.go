package server

import (
	"slices"

	"vigil/internal/task"
)

// Task list order: triggered tasks first (resident ones, then those that only wait for a trigger), then
// scheduled tasks by their next run (soonest first), then paused ones. Ties keep the store's order.
const (
	rankResident = iota
	rankTriggered
	rankScheduled
	rankPaused
)

func viewRank(v TaskView) int {
	switch {
	case v.Task.Mode == task.ModeResident:
		return rankResident
	case !v.Task.Enabled:
		return rankPaused
	case v.NextRun != nil:
		return rankScheduled
	default:
		return rankTriggered
	}
}

// sortViews returns views in task list order, leaving the input as it was.
func sortViews(views []TaskView) []TaskView {
	out := slices.Clone(views)
	slices.SortStableFunc(out, func(a, b TaskView) int {
		if ra, rb := viewRank(a), viewRank(b); ra != rb {
			return ra - rb
		}
		if a.NextRun != nil && b.NextRun != nil {
			return a.NextRun.Compare(*b.NextRun)
		}
		return 0
	})
	return out
}
