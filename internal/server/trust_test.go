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
	got := trustFor(items, memory, nil, "slack", now)
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

// A to-do the owner answered by hand in Slack still says whether the agent had it right: the draft
// (or, when the agent was not to send one, the reply it would have sent) against what the owner wrote.
func TestTrustCountsRepliesWrittenByHandInSlack(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	mk := func(id, reply, shadow string) inbox.Item {
		return inbox.Item{ID: id, TaskID: "slack", Kind: "要排期", Decision: inbox.DecisionHandledElsewhere,
			Reply: reply, Shadow: shadow, Status: inbox.StatusDone, Updated: now.Add(-time.Hour)}
	}
	items := []inbox.Item{
		mk("td-1", "这周排不进，下周三可以开始", ""),
		mk("td-2", "", "这周排不进，下周一开始"),
		mk("td-3", "", "可以，今天就做"),
		mk("td-4", "", "可以，今天就做"), // no hand reply found: says nothing about the draft
	}
	hand := map[string]string{
		"td-1": "这周排不进，下周三可以开始",
		"td-2": "这周排不进，下周一开始吧",
		"td-3": "这个要先问下产品，先别排",
	}

	k := trustFor(items, "", hand, "slack", now)[0]

	if k.Samples != 4 || k.Drafted != 3 || math.Abs(k.Adoption-2.0/3) > 1e-9 {
		t.Fatalf("trust = %+v", k)
	}
}
