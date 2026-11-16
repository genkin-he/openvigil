package server

import (
	"math"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
)

func TestTrustRates(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }
	mk := func(decision, reply string, at time.Time) inbox.Item {
		return inbox.Item{TaskID: "slack", Kind: "问进度", Decision: decision, Reply: reply, Status: inbox.StatusDone, Updated: at}
	}
	items := []inbox.Item{
		mk(inbox.DecisionAsIs, "r", day(1)), mk(inbox.DecisionAsIs, "r", day(2)), mk(inbox.DecisionAsIs, "r", day(3)),
		mk(inbox.DecisionEdited, "r", day(1)),
		mk(inbox.DecisionInstructed, "", day(1)), // no draft: out of the adoption denominator
		mk(inbox.DecisionNoise, "", day(1)),
		mk(inbox.DecisionStale, "", day(1)), mk(inbox.DecisionStale, "", day(1)),
		mk(inbox.DecisionDismissed, "", day(1)), // legacy, reason unknown
		mk(inbox.DecisionAsIs, "r", day(40)),    // outside the window
		{TaskID: "slack", Kind: "只过时", Decision: inbox.DecisionStale, Status: inbox.StatusDone, Updated: day(1)},
		{TaskID: "other", Kind: "问进度", Decision: inbox.DecisionNoise, Status: inbox.StatusDone, Updated: day(1)},
	}
	memory := "- 2026-11-01 我已授权：「问进度」类事情，有证据的明确答案直接以我的口吻回复\n- 2026-11-02 别的"
	got := trustFor(items, memory, "slack", now)
	if len(got) != 1 {
		t.Fatalf("kinds = %+v", got)
	}
	k := got[0]
	if k.Drafted != 4 || k.Adoption != 0.75 || k.Samples != 6 || math.Abs(k.Noise-1.0/6) > 1e-9 {
		t.Fatalf("rates = %+v", k)
	}
	if !k.Authorized || !k.AuthorizedAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)) {
		t.Fatalf("authorization = %+v", k)
	}
}

func TestTrustEndpoint(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	it := addTodo(t, h, `{"task_id":"slack","kind":"通知","title":"x","ref":"C1:1.1"}`)
	if _, err := s.AnswerTodoReason(it.ID, TodoDismiss, "", inbox.DecisionNoise); err != nil {
		t.Fatal(err)
	}
	rec, _ := do(h, "GET", "/api/tasks/slack/trust", "", true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"kind":"通知"`) || !strings.Contains(rec.Body.String(), `"noise":1`) {
		t.Fatalf("trust: %d %s", rec.Code, rec.Body)
	}
}
