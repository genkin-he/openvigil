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

func TestRetroCountsOutputsAndCostPerOutput(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: fix\nname: Fix\ntrigger: {kind: manual}\n"), true)
	now := time.Now()
	for _, r := range []store.RunRecord{
		{ID: "r1", Status: store.StatusOK, CostUSD: 3, Made: []string{"MR https://g/1", "评论 https://s/1"}, StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-time.Hour)},
		{ID: "r2", Status: store.StatusOK, CostUSD: 1, Made: []string{"MR https://g/1"}, StartedAt: now.Add(-30 * time.Minute), EndedAt: now.Add(-30 * time.Minute)}, // same MR again
		{ID: "r3", Status: store.StatusSkipped, StartedAt: now.Add(-10 * time.Minute)},
	} {
		r.TaskID = "fix"
		if err := s.Store.AppendRun(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.aftermathLog().add(Aftermath{Task: "fix", Run: "r1", Channel: "C1", ReplyTS: "1.1", Reply: "好了", At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	taken, _, _ := s.Inbox.Add(inbox.Item{TaskID: "fix", Kind: "问进度", Title: "a", Reply: "草稿", Ref: "c:1"})
	if _, err := s.AnswerTodo(taken.ID, TodoSend, "草稿"); err != nil {
		t.Fatal(err)
	}
	dropped, _, _ := s.Inbox.Add(inbox.Item{TaskID: "fix", Kind: "问进度", Title: "b", Ref: "c:2"})
	s.AnswerTodoReason(dropped.ID, TodoDismiss, "", inbox.DecisionNoise)
	s.Engine.Wait()

	rec, _ := do(h, "GET", "/api/retro?days=7", "", true)
	var env struct {
		Data retroView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	for _, tk := range env.Data.Tasks {
		if tk.ID != "fix" {
			continue
		}
		o := tk.Output
		if o.Made != 2 || o.Replies != 1 || o.TodosTaken != 1 || o.Total != 4 || o.CostPerOutput != 1 {
			t.Fatalf("output = %+v (cost %v)", o, tk.CostUSD)
		}
		return
	}
	t.Fatal("task missing")
}
