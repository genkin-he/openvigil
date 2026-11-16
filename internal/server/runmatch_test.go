package server

import (
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/runner"
	"vigil/internal/store"
)

func TestLiveRunOfPicksTheMainLane(t *testing.T) {
	live := []runner.LiveRun{
		{TaskID: "slack", RunID: "r-job", Job: "j1"},
		{TaskID: "other", RunID: "r-other"},
		{TaskID: "slack", RunID: "r-main"},
	}
	if got := liveRunOf(live, "slack"); got != "r-main" {
		t.Fatalf("liveRunOf = %q", got)
	}
	if got := liveRunOf(live, "none"); got != "" {
		t.Fatalf("no run for task: %q", got)
	}
}

func TestRunOfInfersLegacyItems(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	base := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	for _, r := range []store.RunRecord{
		{ID: "r1", TaskID: "slack", Status: "ok", Input: "SLACK C1:1794.5 thread=- | 问进度", StartedAt: base},
		{ID: "r2", TaskID: "slack", Status: "ok", Input: "SLACK C2:1800.1 thread=- | 别的", StartedAt: base.Add(5 * time.Minute)},
		{ID: "r3", TaskID: "slack", Status: "ok", Input: "SLACK C3:1900.1 | 更晚", StartedAt: base.Add(time.Hour)},
	} {
		if err := s.Store.AppendRun(r); err != nil {
			t.Fatal(err)
		}
	}
	at := base.Add(8 * time.Minute)
	if r, ok := s.runOf(inbox.Item{TaskID: "slack", Ref: "C1:1794.5", Created: at}); !ok || r.ID != "r1" {
		t.Fatalf("by ts: %+v %v", r, ok)
	}
	if r, ok := s.runOf(inbox.Item{TaskID: "slack", Ref: "evolve:x", Created: at}); !ok || r.ID != "r2" {
		t.Fatalf("by time: %+v %v", r, ok)
	}
	if _, ok := s.runOf(inbox.Item{TaskID: "slack", Ref: "evolve:x", Created: base.Add(30 * time.Minute)}); ok {
		t.Fatal("outside the window must not match")
	}
	if r, ok := s.runOf(inbox.Item{TaskID: "slack", RunID: "r3"}); !ok || r.ID != "r3" {
		t.Fatalf("by id: %+v %v", r, ok)
	}
}
