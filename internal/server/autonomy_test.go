package server

import (
	"testing"
	"time"

	"vigil/internal/inbox"
)

func TestAutonomyCountsHowOftenTheOwnerHadToStepIn(t *testing.T) {
	now := time.Date(2026, 11, 19, 20, 0, 0, 0, time.Local)
	day := func(d int) time.Time { return now.AddDate(0, 0, -d) }
	item := func(task, kind, decision, answer string, created time.Time) inbox.Item {
		return inbox.Item{TaskID: task, Kind: kind, Decision: decision, Answer: answer, Created: created}
	}
	items := []inbox.Item{
		item("slack", "要排期", inbox.DecisionAsIs, "", day(0)),
		item("slack", "要排期", inbox.DecisionInstructed, "我选了「下次再排」（排进下周）", day(0)),
		item("slack", "要排期", inbox.DecisionInstructed, "我选了「先不回」。补充：等 MR 合了再说", day(0)),
		item("slack", "要排期", inbox.DecisionInstructed, "让他去找产品", day(0)),
		item("slack", "问数据", inbox.DecisionEdited, "是 5 条", day(0)),
		item("slack", "问数据", inbox.DecisionHandledElsewhere, "", day(1)),
		item("slack", "问数据", inbox.DecisionNoise, "", day(1)),
		{TaskID: "slack", Kind: "问数据", Status: inbox.StatusOpen, Created: day(1)}, // still open
		item("tracker", "缺陷", inbox.DecisionAsIs, "", day(2)),
		item("slack", "要排期", inbox.DecisionInstructed, "太早了，不算", day(9)),
		{TaskID: "slack", Ref: "selftest:t1", Title: "Vigil 自测：待办发送", Created: day(0)}, // Vigil's own test card: not the owner's work
	}

	v := autonomyOf(items, now)

	if len(v.Days) != 3 {
		t.Fatalf("days = %+v", v.Days)
	}
	today := v.Days[0]
	if today.Date != "11-19" || today.Task != "slack" || today.Total != 5 || today.AsIs != 1 || today.Picked != 1 ||
		today.Typed != 2 || today.Edited != 1 || today.Effort != 3 {
		t.Fatalf("today = %+v", today)
	}
	if y := v.Days[1]; y.Date != "11-18" || y.Elsewhere != 1 || y.Dismissed != 1 || y.Open != 1 || y.Effort != 1 {
		t.Fatalf("yesterday = %+v", y)
	}
	if len(v.Kinds) != 3 || v.Kinds[0].Kind != "要排期" || v.Kinds[0].Effort != 2 || v.Kinds[0].Total != 4 {
		t.Fatalf("kinds = %+v", v.Kinds)
	}
	if v.Kinds[2].Task != "tracker" || v.Kinds[2].Effort != 0 {
		t.Fatalf("the kind that needs no one sorts last: %+v", v.Kinds)
	}
}

// 11-24 showed 9 to-dos as "open" that were all settled: dialogs the owner pressed through the card, a
// to-do the task closed after the owner approved on the web, and ones that closed without anyone.
func TestAutonomyCountsSettledToDosWithoutADecision(t *testing.T) {
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, time.Local)
	done := func(ref, outcome string) inbox.Item {
		return inbox.Item{TaskID: "slack", Ref: ref, Status: inbox.StatusDone, Outcome: outcome, Created: now}
	}
	items := []inbox.Item{
		done("dialog:slack", dialogPressed+"Yes」"),
		done("dialog:slack", "agent 换了一个确认框，见新的待办"),
		{TaskID: "slack", Ref: "C1:2", Status: inbox.StatusDone, TaskClosed: true, Decision: inbox.DecisionHandledElsewhere, Outcome: "你已在网页批准", Created: now},
		{TaskID: "slack", Ref: "C1:3", Status: inbox.StatusDone, TaskClosed: true, Outcome: "组长已拍板，和选项一致", Created: now},
		done("check:glab", "已恢复（自检通过）"),
		{TaskID: "slack", Ref: "C1:4", Status: inbox.StatusOpen, Created: now},
	}

	d := autonomyOf(items, now).Days[0]

	if d.Total != 6 || d.Picked != 1 || d.Elsewhere != 1 || d.Auto != 3 || d.Open != 1 || d.Effort != 1 {
		t.Fatalf("day = %+v", d)
	}
}

func TestTaskClosingAToDoTheOwnerHandledHimselfRecordsIt(t *testing.T) {
	s, h := newServer(t)
	it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "要审批", Title: "批一下", Ref: "C1:1"})
	do(h, "POST", "/api/todos/"+it.ID+"/done", `{"outcome":"你已在网页批准","by_owner":true}`, true)
	got, _ := s.Inbox.Get(it.ID)
	if got.Status != inbox.StatusDone || !got.TaskClosed || got.Decision != inbox.DecisionHandledElsewhere {
		t.Fatalf("item = %+v", got)
	}
}

func TestTaskPageShowsItsOwnAutonomy(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	mine, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "要排期", Title: "排期？", Ref: "C1:1"})
	_, _, _ = s.Inbox.Add(inbox.Item{TaskID: "other", Kind: "问数据", Title: "数据？", Ref: "C1:2"})
	if _, err := s.Inbox.Answer(mine.ID, "下周再说"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inbox.SetDecision(mine.ID, inbox.DecisionInstructed); err != nil {
		t.Fatal(err)
	}

	_, env := do(h, "GET", "/api/tasks/slack", "", true)
	got := decodeData[struct {
		Autonomy autonomyView `json:"autonomy"`
	}](t, env.Data).Autonomy
	if len(got.Kinds) != 1 || got.Kinds[0].Kind != "要排期" || got.Kinds[0].Typed != 1 || len(got.Days) != 1 || got.Days[0].Effort != 1 {
		t.Fatalf("autonomy = %+v", got)
	}
}

func TestLedgerMeasuresAKindBeforeAndAfterAChange(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)
	s.Now = func() time.Time { return now }
	settle := func(kind, decision, answer string) {
		t.Helper()
		it, _, err := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: kind, Title: "x", Ref: randHex(4)})
		if err != nil {
			t.Fatal(err)
		}
		if answer != "" {
			_, _ = s.Inbox.Answer(it.ID, answer)
		}
		if _, err := s.Inbox.SetDecision(it.ID, decision); err != nil {
			t.Fatal(err)
		}
	}
	// Before: three scheduling to-dos, all needing typed answers; one other kind that does not count.
	for range 3 {
		settle("要排期", inbox.DecisionInstructed, "下周再说")
	}
	settle("问数据", inbox.DecisionInstructed, "查一下")

	rec, env := do(h, "POST", "/api/evolve/ledger", `{"task":"slack","target":"brief","change":"排期类给选项","why":"td-1","expect":"排期类出手减少","metric":"要排期","review_days":3}`, true)
	if rec.Code != 200 {
		t.Fatalf("record = %d %s", rec.Code, rec.Body)
	}
	e := decodeData[LedgerEntry](t, env.Data)
	if e.Metric != "slack/要排期" || e.Baseline == nil || e.Baseline.Total != 3 || e.Baseline.Effort != 3 {
		t.Fatalf("entry = %+v baseline %+v", e, e.Baseline)
	}

	// After: the inbox clock moves on; two scheduling to-dos, one picked from buttons.
	now = now.Add(time.Hour)
	settle("要排期", inbox.DecisionInstructed, "我选了「下周排」")
	settle("要排期", inbox.DecisionInstructed, "不做")
	now = now.Add(4 * 24 * time.Hour)

	_, env = do(h, "GET", "/api/evolve/signals?since=2000-01-01T00:00:00Z", "", true)
	view := decodeData[signalsView](t, env.Data)
	if len(view.LedgerDue) != 1 || view.LedgerDue[0].After == nil {
		t.Fatalf("due = %+v", view.LedgerDue)
	}
	if a := view.LedgerDue[0].After; a.Total != 2 || a.Picked != 1 || a.Effort != 1 {
		t.Fatalf("after = %+v", a)
	}
}

func TestReviewDatesSpreadOut(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)
	s.Now = func() time.Time { return now }
	days := map[string]int{}
	for range ledgerPerDay + 2 {
		_, env := do(h, "POST", "/api/evolve/ledger", `{"task":"slack","target":"m","change":"c","why":"w","expect":"e","review_days":3}`, true)
		days[decodeData[LedgerEntry](t, env.Data).ReviewAt.Format("01-02")]++
	}
	if days["11-22"] != ledgerPerDay || days["11-23"] != 2 {
		t.Fatalf("review days = %v", days)
	}
}
