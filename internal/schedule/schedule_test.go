package schedule

import (
	"testing"
	"time"

	"vigil/internal/task"
)

var base = time.Date(2026, 11, 10, 10, 0, 0, 0, time.Local)

func cronTask(expr, catchUp string) task.Task {
	return task.WithDefaults(task.Task{ID: "a", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerCron, Cron: expr}, Run: task.Run{CatchUp: catchUp}})
}

func TestDecideRecurring(t *testing.T) {
	hourly := cronTask("0 * * * *", task.CatchUpOnce)
	skip := cronTask("0 * * * *", task.CatchUpSkip)
	cases := []struct {
		name   string
		tk     task.Task
		last   time.Time
		now    time.Time
		run    bool
		missed bool
	}{
		{"baseline does not run", hourly, time.Time{}, base, false, false},
		{"not yet due", hourly, base, base.Add(59 * time.Minute), false, false},
		{"due on time", hourly, base, base.Add(time.Hour + 10*time.Second), true, false},
		{"slept through three hours catches up once", hourly, base, base.Add(3*time.Hour + 30*time.Minute), true, true},
		{"slept with skip policy", skip, base, base.Add(3*time.Hour + 30*time.Minute), false, true},
		{"disabled", task.Task{ID: "a", Trigger: task.Trigger{Kind: task.TriggerCron, Cron: "* * * * *"}}, base, base.Add(time.Hour), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.tk, c.last, false, c.now)
			if d.Run != c.run || d.Missed != c.missed {
				t.Fatalf("Decide = %+v", d)
			}
		})
	}
}

func TestDecideEvery(t *testing.T) {
	tk := task.WithDefaults(task.Task{ID: "a", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerEvery, Every: task.Duration(30 * time.Minute)}})
	if d := Decide(tk, base, false, base.Add(29*time.Minute)); d.Run {
		t.Fatal("ran early")
	}
	if d := Decide(tk, base, false, base.Add(30*time.Minute)); !d.Run || d.Missed {
		t.Fatalf("Decide = %+v", d)
	}
}

func TestDecideOnce(t *testing.T) {
	tk := task.WithDefaults(task.Task{ID: "a", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerOnce, At: base}})
	if d := Decide(tk, time.Time{}, false, base.Add(-time.Second)); d.Run {
		t.Fatal("ran before At")
	}
	if d := Decide(tk, time.Time{}, false, base.Add(time.Second)); !d.Run || !d.OnceFinal {
		t.Fatalf("Decide = %+v", d)
	}
	if d := Decide(tk, time.Time{}, true, base.Add(time.Hour)); d.Run {
		t.Fatal("ran twice")
	}
}

func TestValidateCron(t *testing.T) {
	if ValidateCron("37 10-18 * * 1-5") != nil || ValidateCron("@hourly") != nil {
		t.Fatal("valid cron rejected")
	}
	if ValidateCron("61 * * * *") == nil {
		t.Fatal("invalid cron accepted")
	}
}

func TestEveryDoesNotDrift(t *testing.T) {
	tk := task.WithDefaults(task.Task{ID: "a", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerEvery, Every: task.Duration(time.Hour)}})
	d := Decide(tk, base, false, base.Add(time.Hour+14*time.Second))
	if !d.Run || !d.Mark.Equal(base.Add(time.Hour)) {
		t.Fatalf("mark should be the fire time, got %+v", d)
	}
}
