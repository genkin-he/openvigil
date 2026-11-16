// Package daemon runs Vigil's long-lived loops: scheduling, event streams, healing and sleep prevention.
package daemon

import (
	"context"
	"time"

	"vigil/internal/runner"
	"vigil/internal/schedule"
	"vigil/internal/store"
	"vigil/internal/task"
)

// SleepGap is how long between two ticks counts as the machine having slept.
const SleepGap = 2 * time.Minute

// Tick checks every time-triggered task once. It returns true when the gap since prev suggests the machine slept.
func Tick(s *store.Store, e *runner.Engine, now, prev time.Time, logf func(string, ...any)) bool {
	woke := !prev.IsZero() && now.Sub(prev) > SleepGap
	if woke {
		logf("woke up after %s, checking missed runs", now.Sub(prev).Round(time.Second))
	}
	tasks, errs := s.List()
	for _, err := range errs {
		logf("load task: %v", err)
	}
	for _, t := range tasks {
		tickTask(s, e, t, now, logf)
	}
	return woke
}

func tickTask(s *store.Store, e *runner.Engine, t task.Task, now time.Time, logf func(string, ...any)) {
	switch t.Trigger.Kind {
	case task.TriggerCron, task.TriggerEvery, task.TriggerOnce:
	default:
		return
	}
	st, err := s.State(t.ID)
	if err != nil {
		logf("state %s: %v", t.ID, err)
		return
	}
	d := schedule.Decide(t, st.LastScheduledAt, st.OnceDone, now)
	if d.Mark.IsZero() {
		return
	}
	if _, err := s.UpdateState(t.ID, func(st store.State) store.State {
		st.LastScheduledAt = d.Mark
		if d.OnceFinal {
			st.OnceDone = true
		}
		return st
	}); err != nil {
		logf("state %s: %v", t.ID, err)
		return
	}
	if !d.Run {
		if d.Missed {
			logf("%s: missed run skipped (catch_up=skip)", t.ID)
		}
		return
	}
	trigger := t.Trigger.Kind
	if d.Missed {
		trigger = runner.ByCatchUp
	}
	e.Submit(runner.Request{TaskID: t.ID, Trigger: trigger})
}

// RunScheduler ticks every interval until ctx ends.
func RunScheduler(ctx context.Context, s *store.Store, e *runner.Engine, interval time.Duration, logf func(string, ...any)) {
	prev := time.Time{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		now := time.Now()
		Tick(s, e, now, prev, logf)
		prev = now
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
