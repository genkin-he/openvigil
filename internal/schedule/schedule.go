// Package schedule decides when time-triggered tasks are due, including runs missed while the machine slept.
package schedule

import (
	"time"

	"vigil/internal/task"
)

// MissGrace is how late a fire time may be before it counts as missed (machine asleep, daemon down).
const MissGrace = 2 * time.Minute

// Decision says what to do with a task at a given moment.
type Decision struct {
	Run       bool      // start a run now
	Missed    bool      // the fire time was missed by more than MissGrace
	Mark      time.Time // new LastScheduledAt to record (zero = unchanged)
	Reason    string
	OnceFinal bool // a once task is finished after this decision
}

// ValidateCron reports whether expr is a supported cron expression.
func ValidateCron(expr string) error {
	_, err := task.ParseCron(expr)
	return err
}

// Next returns the first fire time strictly after `after`. ok is false for triggers without a timetable.
func Next(t task.Task, after time.Time) (time.Time, bool) {
	switch t.Trigger.Kind {
	case task.TriggerCron:
		sched, err := task.ParseCron(t.Trigger.Cron)
		if err != nil {
			return time.Time{}, false
		}
		return sched.Next(after), true
	case task.TriggerEvery:
		return after.Add(t.Trigger.Every.Std()), true
	case task.TriggerOnce:
		if t.Trigger.At.After(after) {
			return t.Trigger.At, true
		}
		return time.Time{}, false
	}
	return time.Time{}, false
}

// Decide looks at a task's timetable since lastScheduled and says whether it should run at now.
// A zero lastScheduled means the task was just enabled: it starts counting from now.
func Decide(t task.Task, lastScheduled time.Time, onceDone bool, now time.Time) Decision {
	if !t.Enabled {
		return Decision{}
	}
	switch t.Trigger.Kind {
	case task.TriggerOnce:
		return decideOnce(t, onceDone, now)
	case task.TriggerCron, task.TriggerEvery:
		return decideRecurring(t, lastScheduled, now)
	}
	return Decision{}
}

func decideOnce(t task.Task, done bool, now time.Time) Decision {
	if done || now.Before(t.Trigger.At) {
		return Decision{}
	}
	missed := now.Sub(t.Trigger.At) > MissGrace
	if missed && t.Run.CatchUp == task.CatchUpSkip {
		return Decision{Missed: true, Mark: now, OnceFinal: true, Reason: "missed one-off run skipped"}
	}
	return Decision{Run: true, Missed: missed, Mark: now, OnceFinal: true, Reason: reason(missed, "scheduled")}
}

func decideRecurring(t task.Task, last, now time.Time) Decision {
	if last.IsZero() {
		return Decision{Mark: now, Reason: "baseline"}
	}
	next, ok := Next(t, last)
	if !ok || next.After(now) {
		return Decision{}
	}
	// Collapse every fire time missed since `last` into a single decision about the latest one.
	latest := next
	for {
		n, ok := Next(t, latest)
		if !ok || n.After(now) {
			break
		}
		latest = n
	}
	missed := now.Sub(latest) > MissGrace
	if missed && t.Run.CatchUp == task.CatchUpSkip {
		return Decision{Missed: true, Mark: now, Reason: "missed run skipped"}
	}
	mark := latest
	if missed {
		mark = now
	}
	return Decision{Run: true, Missed: missed, Mark: mark, Reason: reason(missed, "scheduled")}
}

func reason(missed bool, base string) string {
	if missed {
		return "catch-up"
	}
	return base
}
