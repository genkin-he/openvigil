package server

import (
	"slices"
	"testing"
	"time"

	"vigil/internal/task"
)

func TestTaskListPutsTriggeredFirstThenSoonestRun(t *testing.T) {
	now := time.Unix(1794884400, 0)
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	views := []TaskView{
		{Task: task.Task{ID: "weekly", Enabled: true}, NextRun: at(72 * time.Hour)},
		{Task: task.Task{ID: "paused", Enabled: false}},
		{Task: task.Task{ID: "events", Enabled: true}}, // waits for a trigger, no next run
		{Task: task.Task{ID: "disk", Enabled: true}, NextRun: at(5 * time.Hour)},
		{Task: task.Task{ID: "slack", Enabled: true, Mode: task.ModeResident}},
		{Task: task.Task{ID: "brief", Enabled: true}, NextRun: at(18 * time.Hour)},
	}

	got := make([]string, 0, len(views))
	for _, v := range sortViews(views) {
		got = append(got, v.Task.ID)
	}

	want := []string{"slack", "events", "disk", "brief", "weekly", "paused"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
