package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/replay"
	"vigil/internal/store"
)

// fakeReplayer answers "reply 已修好" when the rules carry the new text, "ignore" otherwise.
func fakeReplayer(calls *atomic.Int32) replay.Runner {
	return replay.Runner{Model: "sonnet", Run: func(_ context.Context, _ string, _ []string, stdin string) ([]byte, error) {
		calls.Add(1)
		if strings.Contains(stdin, "NEWRULE") {
			return []byte(`{"structured_output":{"action":"reply","text":"已修好了","why":"新规则"},"total_cost_usd":0.01}`), nil
		}
		return []byte(`{"structured_output":{"action":"ignore","why":"旧规则"},"total_cost_usd":0.01}`), nil
	}}
}

func replayServer(t *testing.T, runs int) (*Server, *atomic.Int32) {
	t.Helper()
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	if err := s.Store.WriteDoc("slack", "brief.md", "规则：OLDRULE"); err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	s.Replayer = fakeReplayer(calls)
	base := s.Now().Add(-2 * time.Hour)
	for i := range runs {
		if err := s.Store.AppendRun(store.RunRecord{ID: fmt.Sprintf("r%02d", i), TaskID: "slack", Status: "ok",
			Input: fmt.Sprintf("SLACK C1:%d.1 | 第 %d 条", i, i), Summary: "已回复同事", StartedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	return s, calls
}

func TestReplayBuildsReport(t *testing.T) {
	s, calls := replayServer(t, 3)
	it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "问进度", Title: "x", Reply: "已经修好了", RunID: "r01", Ref: "C1:1.1"})
	if _, err := s.AnswerTodo(it.ID, TodoSend, "已经修好了"); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "brief.md", Before: "OLDRULE", After: "NEWRULE",
		Runs: []string{"r00", "r01", "r02"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 6 || len(rep.Rows) != 3 || rep.Closer < 1 || !strings.Contains(rep.Summary, "回放 3 轮") {
		t.Fatalf("report = %+v (calls %d)", rep, calls.Load())
	}
	var row ReplayRow
	for _, r := range rep.Rows {
		if r.RunID == "r01" {
			row = r
		}
	}
	if row.Verdict != replay.Closer || row.ActualFromSummary {
		t.Fatalf("row with a to-do = %+v", row)
	}
	for _, r := range rep.Rows {
		if r.RunID != "r01" && !r.ActualFromSummary {
			t.Fatalf("row without a to-do must be marked as inferred: %+v", r)
		}
	}
	got, err := s.loadReplay(rep.ID)
	if err != nil || got.Summary != rep.Summary {
		t.Fatalf("saved report = %+v %v", got, err)
	}
}

func TestReplayRejectsBeforeNotInDoc(t *testing.T) {
	s, _ := replayServer(t, 1)
	if _, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "brief.md", Before: "不存在的段落", After: "x"}); err == nil {
		t.Fatal("a before text not in the doc must be refused")
	}
	if _, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "task.yaml", Before: "", After: "x"}); err == nil {
		t.Fatal("only brief, memory and runbook can be replayed")
	}
}

func TestReplayAppendsWhenBeforeIsEmpty(t *testing.T) {
	s, calls := replayServer(t, 1)
	rep, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "memory.md", After: "NEWRULE", Runs: []string{"r00"}})
	if err != nil || calls.Load() != 2 || rep.Rows[0].After.Action != replay.ActionReply {
		t.Fatalf("append replay = %+v %v", rep, err)
	}
}

func TestReplayMarksMissingEvidence(t *testing.T) {
	s, _ := replayServer(t, 1)
	rep, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "brief.md", Before: "OLDRULE", After: "NEWRULE", Runs: []string{"r00"}})
	if err != nil || len(rep.Rows) != 1 || !rep.Rows[0].NoEvidence {
		t.Fatalf("rows = %+v %v", rep.Rows, err)
	}
}

func TestReplayPicksAtMostTwenty(t *testing.T) {
	s, calls := replayServer(t, 30)
	rep, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "brief.md", Before: "OLDRULE", After: "NEWRULE"})
	if err != nil || len(rep.Rows) != replayMaxRuns || calls.Load() != 2*replayMaxRuns {
		t.Fatalf("rows %d, calls %d, %v", len(rep.Rows), calls.Load(), err)
	}
}

func TestReplayAPIRunsInBackground(t *testing.T) {
	s, _ := replayServer(t, 2)
	h := s.Handler()
	rec, _ := do(h, "POST", "/api/evolve/replay", `{"task":"slack","doc":"brief.md","before":"OLDRULE","after":"NEWRULE"}`, true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"rp-`) {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	s.Wait()
	id := strings.SplitN(strings.SplitN(rec.Body.String(), `"id":"`, 2)[1], `"`, 2)[0]
	rec, _ = do(h, "GET", "/api/evolve/replay/"+id, "", true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"done"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
}

// replayerSaying makes every replay under the new rules answer act, and the old rules reply with the draft.
func replayerSaying(act replay.Action) replay.Runner {
	return replay.Runner{Model: "sonnet", Run: func(_ context.Context, _ string, _ []string, stdin string) ([]byte, error) {
		if strings.Contains(stdin, "我已授权：「问进度」") {
			return []byte(`{"structured_output":{"action":"` + string(act) + `","text":"草稿","why":"x"},"total_cost_usd":0.01}`), nil
		}
		return []byte(`{"structured_output":{"action":"reply","text":"草稿","why":"x"},"total_cost_usd":0.01}`), nil
	}}
}

// proposeAfterFive settles five drafted 问进度 to-dos as is, each tied to its own round.
func proposeAfterFive(t *testing.T, s *Server) {
	t.Helper()
	for i := range 5 {
		id := fmt.Sprintf("p%d", i)
		if err := s.Store.AppendRun(store.RunRecord{ID: id, TaskID: "slack", Status: "ok", Input: "问进度 " + id, StartedAt: s.Now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "问进度", Title: id, Reply: "草稿", Ref: "P:" + id, RunID: id})
		if _, err := s.AnswerTodo(it.ID, TodoSend, "草稿"); err != nil {
			t.Fatal(err)
		}
	}
	s.Wait()
	s.Engine.Wait()
}

func TestProposalHeldWhenReplayIsFarther(t *testing.T) {
	s, _ := replayServer(t, 0)
	s.Replayer = replayerSaying(replay.ActionIgnore) // owner replied; the rule would make it ignore: farther
	proposeAfterFive(t, s)
	if openRule(s, "rule:slack:问进度:as_is") {
		t.Fatal("a suggestion that replays farther must be held back")
	}
	rep, _ := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "memory.md", After: "x", Kind: "问进度"})
	for _, r := range rep.Rows {
		if !strings.HasPrefix(r.RunID, "p") {
			t.Fatalf("a round that carried out an answer was replayed: %+v", r)
		}
	}
	entries, _ := s.ledger().list()
	if len(entries) != 1 || entries[0].Status != LedgerRejected {
		t.Fatalf("ledger = %+v", entries)
	}
}

func TestProposalCarriesReplaySummary(t *testing.T) {
	s, _ := replayServer(t, 0)
	s.Replayer = replayerSaying(replay.ActionReply)
	proposeAfterFive(t, s)
	it, ok, _ := s.Inbox.OpenByRef("rule:slack:问进度:as_is")
	if !ok || !strings.Contains(it.Analysis, "回放") || !strings.HasPrefix(it.Link, "/api/evolve/replay/rp-") {
		t.Fatalf("suggestion = %+v %v", it, ok)
	}
}

// Final review #1: "I handled it elsewhere" is a case that needed the owner, never support for "stop reporting".
func TestHandledElsewhereIsNotEvidenceForQuiet(t *testing.T) {
	if d := actualFromTodo(inbox.Item{Decision: inbox.DecisionHandledElsewhere}); d.Action != replay.ActionTodo || !d.Valid {
		t.Fatalf("handled elsewhere = %+v, want a valid todo", d)
	}
	for _, dec := range []string{inbox.DecisionStale, inbox.DecisionDismissed} {
		if d := actualFromTodo(inbox.Item{Decision: dec}); d.Valid {
			t.Fatalf("%s says nothing about what should happen: %+v", dec, d)
		}
	}
	ignore := replay.Decision{Action: replay.ActionIgnore, Valid: true}
	todo := replay.Decision{Action: replay.ActionTodo, Valid: true}
	if v := replay.Compare(todo, ignore, actualFromTodo(inbox.Item{Decision: inbox.DecisionHandledElsewhere})); v == replay.Closer {
		t.Fatal("a quiet rule must not look closer on a handled-elsewhere round")
	}
}

// Final review #2: a held-back suggestion is not replayed again on the next decision.
func TestRejectedProposalIsNotReplayedAgain(t *testing.T) {
	s, _ := replayServer(t, 0)
	var calls atomic.Int32
	inner := replayerSaying(replay.ActionIgnore)
	s.Replayer = replay.Runner{Model: "sonnet", Run: func(ctx context.Context, dir string, args []string, stdin string) ([]byte, error) {
		calls.Add(1)
		return inner.Run(ctx, dir, args, stdin)
	}}
	proposeAfterFive(t, s)
	first := calls.Load()
	if first == 0 {
		t.Fatal("no replay ran")
	}
	settleN(t, s, "问进度", 1, inbox.DecisionAsIs)
	s.Wait()
	s.Engine.Wait() // the rounds carrying out the answers write into the task dir
	if calls.Load() != first {
		t.Fatalf("held-back suggestion replayed again: %d → %d calls", first, calls.Load())
	}
}

// Final review #2: while a suggestion's replay is running, another decision does not start a second one.
func TestProposalReplayIsNotStartedTwice(t *testing.T) {
	s, _ := replayServer(t, 0)
	release := make(chan struct{})
	var calls atomic.Int32
	s.Replayer = replay.Runner{Model: "sonnet", Run: func(context.Context, string, []string, string) ([]byte, error) {
		calls.Add(1)
		<-release
		return []byte(`{"structured_output":{"action":"reply","text":"草稿","why":"x"}}`), nil
	}}
	it := inbox.Item{TaskID: "slack", Ref: "rule:slack:问进度:as_is", Reply: "我已授权：「问进度」类", Title: "t"}
	if err := s.Store.AppendRun(store.RunRecord{ID: "q1", TaskID: "slack", Status: "ok", Input: "x", StartedAt: s.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s.gateProposal(it, "问进度")
	s.gateProposal(it, "问进度")
	close(release)
	s.Wait()
	if calls.Load() != 2 { // one replay = before + after for its single round
		t.Fatalf("calls = %d, want a single replay", calls.Load())
	}
}

// Final review #3: with a kind given, that kind's rounds come first, ahead of other kinds' reworked to-dos.
func TestReplayPicksKindFirst(t *testing.T) {
	s, _ := replayServer(t, 0)
	for i := range 25 {
		id := fmt.Sprintf("o%02d", i)
		_ = s.Store.AppendRun(store.RunRecord{ID: id, TaskID: "slack", Status: "ok", Input: id, StartedAt: s.Now().Add(-time.Hour)})
		s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "别的", Title: id, Ref: "O:" + id, RunID: id})
		it, _ := s.Inbox.Get(mustRef(t, s, "O:"+id))
		s.Inbox.SetDecision(it.ID, inbox.DecisionInstructed)
	}
	_ = s.Store.AppendRun(store.RunRecord{ID: "k1", TaskID: "slack", Status: "ok", Input: "k1", StartedAt: s.Now().Add(-time.Hour)})
	s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "问进度", Title: "k1", Ref: "K:1", RunID: "k1"})
	k, _ := s.Inbox.Get(mustRef(t, s, "K:1"))
	s.Inbox.SetDecision(k.ID, inbox.DecisionAsIs)
	rep, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "memory.md", After: "NEWRULE", Kind: "问进度"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rep.Rows, func(r ReplayRow) bool { return r.RunID == "k1" }) {
		t.Fatalf("the kind's own round was crowded out: %d rows", len(rep.Rows))
	}
}

func mustRef(t *testing.T, s *Server, ref string) string {
	t.Helper()
	it, ok, err := s.Inbox.OpenByRef(ref)
	if !ok || err != nil {
		t.Fatalf("no item %s", ref)
	}
	return it.ID
}

// Final review #4: a replay cut off (daemon stopping) is failed, not done, and does not raise a suggestion.
func TestCanceledReplayIsFailedAndRaisesNothing(t *testing.T) {
	s, _ := replayServer(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Replayer = replay.Runner{Model: "sonnet", Run: func(ctx context.Context, _ string, _ []string, _ string) ([]byte, error) {
		return nil, ctx.Err()
	}}
	rep, err := s.Replay(ctx, ReplayRequest{Task: "slack", Doc: "brief.md", Before: "OLDRULE", After: "NEWRULE"})
	if err == nil || rep.Status != "failed" {
		t.Fatalf("canceled replay = %+v, %v", rep, err)
	}
	s.Base = ctx
	s.gateProposal(inbox.Item{TaskID: "slack", Ref: "rule:slack:x:as_is", Reply: "r", Title: "t"}, "x")
	s.Wait()
	if openRule(s, "rule:slack:x:as_is") {
		t.Fatal("a failed replay must not let the suggestion through")
	}
}

// Final review #4: each replay call has its own deadline, so a hung claude cannot stall a replay forever.
func TestDecideHasADeadline(t *testing.T) {
	r := replay.Runner{Model: "sonnet", Run: func(ctx context.Context, _ string, _ []string, _ string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("no deadline on a replay call")
		}
		return []byte(`{"structured_output":{"action":"ignore","why":"x"}}`), nil
	}}
	if _, _, err := r.Decide(context.Background(), "rules", replay.Case{RunID: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplayChangesTheOwnersPrinciples(t *testing.T) {
	s, calls := replayServer(t, 2)
	if err := s.Store.WriteOwner("- 2026-11-20 原则：OLDRULE\n"); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Replay(context.Background(), ReplayRequest{Task: "slack", Doc: "owner.md", Before: "OLDRULE", After: "NEWRULE",
		Runs: []string{"r00", "r01"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || rep.Doc != "owner.md" || len(rep.Rows) != 2 {
		t.Fatalf("report = %+v (calls %d)", rep, calls.Load())
	}
	for _, r := range rep.Rows {
		if r.After.Action != replay.ActionReply || r.Before.Action != replay.ActionIgnore {
			t.Fatalf("the changed principle must reach the replayed rules: %+v", r)
		}
	}
	if got, _ := s.Store.ReadOwner(); !strings.Contains(got, "OLDRULE") {
		t.Fatalf("a replay must not write owner.md: %q", got)
	}
}

func TestReplayRulesCarryTheOwnersPrinciples(t *testing.T) {
	s, _ := replayServer(t, 1)
	if err := s.Store.WriteOwner("- 2026-11-20 OWNERLINE\n"); err != nil {
		t.Fatal(err)
	}
	before, after, err := s.replayRules(ReplayRequest{Task: "slack", Doc: "brief.md", Before: "OLDRULE", After: "NEWRULE"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "OWNERLINE") || !strings.Contains(after, "OWNERLINE") || !strings.Contains(after, "NEWRULE") {
		t.Fatalf("rules must hold owner.md whatever doc is replayed:\nbefore=%s\nafter=%s", before, after)
	}
	appended, _, err := s.replayRules(ReplayRequest{Task: "slack", Doc: "owner.md", After: "- 2026-11-26 NEWLINE"})
	if err != nil || strings.Contains(appended, "NEWLINE") {
		t.Fatalf("appending a principle: before must not hold it (%v)", err)
	}
	if _, _, err := s.replayRules(ReplayRequest{Task: "slack", Doc: "task.yaml", After: "x"}); err == nil {
		t.Fatal("other files must still be refused")
	}
}
