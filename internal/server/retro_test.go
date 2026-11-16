package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/store"
)

func TestRetroSummarisesRunsAndTodos(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	now := time.Now()
	for i, r := range []store.RunRecord{
		{ID: "r1", Status: store.StatusOK, CostUSD: 0.5, StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-50 * time.Minute)},
		{ID: "r2", Status: store.StatusFail, Error: "claude exited: exit status 143", StartedAt: now.Add(-2 * time.Hour), EndedAt: now.Add(-2 * time.Hour)},
		{ID: "r3", Status: store.StatusFail, Error: "claude exited: exit status 143", StartedAt: now.Add(-3 * time.Hour), EndedAt: now.Add(-3 * time.Hour)},
		{ID: "r4", Status: store.StatusOK, Model: "haiku", StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-time.Hour)},
		{ID: "old", Status: store.StatusFail, StartedAt: now.Add(-30 * 24 * time.Hour)},
	} {
		r.TaskID = "slack"
		if err := s.Store.AppendRun(r); err != nil {
			t.Fatal(i, err)
		}
	}
	it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "问进度", Title: "小林：进度", Reply: "今天", Ref: "c:1"})
	if _, err := s.AnswerTodo(it.ID, TodoSend, "今天"); err != nil {
		t.Fatal(err)
	}
	s.Engine.Wait()

	rec, _ := do(h, "GET", "/api/retro?days=7", "", true)
	var env struct {
		Data retroView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err, rec.Body)
	}
	v := env.Data
	var slack retroTask
	for _, tk := range v.Tasks {
		if tk.ID == "slack" {
			slack = tk
		}
	}
	// the answer run added one more ok run
	if slack.ByStatus[store.StatusFail] != 2 || slack.Light != 1 || slack.CostUSD != 0.5 {
		t.Fatalf("task = %+v", slack)
	}
	if len(slack.Failures) != 1 || slack.Failures[0].Count != 2 || !strings.Contains(slack.Failures[0].Text, "exit status #") {
		t.Fatalf("causes = %+v", slack.Failures)
	}
	if len(v.Kinds) != 1 || v.Kinds[0].ByDecision[inbox.DecisionAsIs] != 1 {
		t.Fatalf("kinds = %+v", v.Kinds)
	}
	if rec, _ := do(h, "GET", "/api/retro?days=99", "", true); rec.Code != 400 {
		t.Fatalf("days=99 accepted: %d", rec.Code)
	}
}
