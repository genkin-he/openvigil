package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/store"
)

func TestTaskPageShowsItsEvolution(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nevolve: propose\ntrigger: {kind: manual}\n"), true)
	do(h, "POST", "/api/tasks", applyJSON("id: other\nname: Other\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.Local)
	s.Now = func() time.Time { return now }

	record := func(task, metric string) LedgerEntry {
		t.Helper()
		body := `{"task":"` + task + `","target":"memory.md","change":"加一条教训","why":"td-1","expect":"同类出手减少","metric":"` + metric + `"}`
		rec, env := do(h, "POST", "/api/evolve/ledger", body, true)
		if rec.Code != 200 {
			t.Fatalf("record = %d %s", rec.Code, rec.Body)
		}
		return decodeData[LedgerEntry](t, env.Data)
	}
	kept := record("slack", "")
	now = now.Add(time.Hour)
	watched := record("slack", "要排期")
	record("other", "")
	do(h, "POST", "/api/evolve/ledger/"+kept.ID, `{"status":"kept","verdict":"同类纠错归零"}`, true)

	// A scheduling to-do after the change, answered with a button: measured as "since the change".
	now = now.Add(time.Hour)
	it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "要排期", Title: "排期？", Ref: "C1:1"})
	_, _ = s.Inbox.Answer(it.ID, "我选了「下周」")
	_, _ = s.Inbox.SetDecision(it.ID, inbox.DecisionInstructed)

	_, env := do(h, "GET", "/api/tasks/slack", "", true)
	ev := decodeData[struct {
		Evolution evolutionView `json:"evolution"`
	}](t, env.Data).Evolution
	if ev.Policy != "propose" || ev.Counts[LedgerKept] != 1 || ev.Counts[LedgerWatching] != 1 || len(ev.Changes) != 2 {
		t.Fatalf("evolution = %+v", ev)
	}
	if ev.Changes[0].ID != watched.ID || ev.Changes[0].Since == nil || ev.Changes[0].Since.Picked != 1 {
		t.Fatalf("newest first, measured since the change: %+v", ev.Changes[0])
	}
	if ev.Changes[1].Since != nil {
		t.Fatalf("a change without a metric has no measure: %+v", ev.Changes[1])
	}
}

func TestTheEvolutionTaskCannotBeRemoved(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: evo\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	do(h, "POST", "/api/tasks", applyJSON("id: other\nname: Other\ntrigger: {kind: manual}\n"), true)
	s.Evolve = (&fakeEvolve{}).evolver("evo")

	if rec, _ := do(h, "DELETE", "/api/tasks/evo", "", true); rec.Code != http.StatusForbidden {
		t.Fatalf("delete evolution task = %d", rec.Code)
	}
	if _, err := s.Store.Get("evo"); err != nil {
		t.Fatal("the evolution task is gone")
	}
	_, env := do(h, "GET", "/api/tasks/evo", "", true)
	if v := decodeData[struct {
		View struct {
			Reserved bool `json:"reserved"`
		} `json:"view"`
	}](t, env.Data); !v.View.Reserved {
		t.Fatal("the task page does not know it is reserved")
	}
	if rec, _ := do(h, "DELETE", "/api/tasks/other", "", true); rec.Code != 200 {
		t.Fatalf("delete an ordinary task = %d", rec.Code)
	}
}

func TestTaskPageListsTheEvolutionRoundsAboutIt(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: evo\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	s.Evolve = (&fakeEvolve{}).evolver("evo")
	at := time.Date(2026, 11, 20, 15, 0, 0, 0, time.Local)
	for i, job := range []string{"evolve-slack", "evolve-tracker", "", "evolve-slack"} {
		rec := store.RunRecord{ID: fmt.Sprintf("r%d", i), TaskID: "evo", Trigger: "job", Status: store.StatusOK, Job: job,
			Summary: "第 " + fmt.Sprint(i) + " 轮", StartedAt: at.Add(time.Duration(i) * time.Hour)}
		if err := s.Store.AppendRun(rec); err != nil {
			t.Fatal(err)
		}
	}
	_, env := do(h, "GET", "/api/tasks/slack", "", true)
	ev := decodeData[struct {
		Evolution evolutionView `json:"evolution"`
	}](t, env.Data).Evolution
	if len(ev.Rounds) != 2 || ev.Rounds[0].ID != "r3" || ev.Rounds[1].ID != "r0" {
		t.Fatalf("rounds about slack = %+v", ev.Rounds)
	}
}

func TestDecidingARunbookProposalHandsTheFollowThroughToEvolution(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: evo\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")
	s.Evolve.PerDay = 8
	name, err := s.Store.SaveProposal("slack", "# 新 runbook\n", "把 22 条纠错提炼成四组做法，批准后删掉这 22 行", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := do(h, "POST", "/api/tasks/slack/proposals/"+name, `{"approve":true}`, true); rec.Code != 200 {
		t.Fatalf("approve = %d", rec.Code)
	}
	f.fireAll()
	if len(f.tasks) != 1 || f.tasks[0] != "slack" || !strings.Contains(f.inputs[0], "批准") ||
		!strings.Contains(f.inputs[0], "删掉这 22 行") {
		t.Fatalf("rounds = %v %q", f.tasks, f.inputs)
	}
}
