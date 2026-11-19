package server

import (
	"slices"

	"vigil/internal/task"
)

// Task list order: resident tasks first, then scheduled tasks by their next run (soonest first), then tasks
// that only wait for a trigger, then paused ones. Ties keep the store's order.
const (
	rankResident = iota
	rankScheduled
	rankTriggered
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
