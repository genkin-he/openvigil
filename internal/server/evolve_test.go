package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vigil/internal/inbox"
)

type fakeEvolve struct {
	mu     sync.Mutex
	inputs []string
	timers []func()
}

func (f *fakeEvolve) evolver(task string) *Evolver {
	return &Evolver{Task: task, PerDay: 2,
		Submit: func(in string) { f.mu.Lock(); f.inputs = append(f.inputs, in); f.mu.Unlock() },
		After:  func(_ time.Duration, fn func()) { f.mu.Lock(); f.timers = append(f.timers, fn); f.mu.Unlock() }}
}

func (f *fakeEvolve) fireAll() {
	f.mu.Lock()
	ts := f.timers
	f.timers = nil
	f.mu.Unlock()
	for _, fn := range ts {
		fn()
	}
}

func TestEvolverBatchesABurstAndCapsRoundsPerDay(t *testing.T) {
	f := &fakeEvolve{}
	e := f.evolver("evo")
	e.Wake("纠错 A")
	e.Wake("纠错 B") // same burst: no second timer
	if len(f.timers) != 1 {
		t.Fatalf("timers = %d", len(f.timers))
	}
	f.fireAll()
	if len(f.inputs) != 1 || !strings.Contains(f.inputs[0], "纠错 A") || !strings.Contains(f.inputs[0], "纠错 B") {
		t.Fatalf("inputs = %q", f.inputs)
	}
	e.Wake("C")
	f.fireAll()
	e.Wake("D") // third round today: over the cap, waits for the daily round
	f.fireAll()
	if len(f.inputs) != 2 {
		t.Fatalf("rounds = %d, want the cap of 2", len(f.inputs))
	}
}

func correctionLine(channel, ts, thread, self, text string) string {
	return fmt.Sprintf("SLACK %s:%s thread=%s user=U1 dm=0 mention=1 self=%s | %s", channel, ts, thread, self, text)
}

func TestCorrectionIsRecordedPassedOnAndWakesEvolution(t *testing.T) {
	s, _ := mirrorServer(t)
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")

	if s.InterceptSlack("slack", correctionLine("C9", "2.0", "1.0", "1", "<@U0SELF> ai 回错了，应该是 sit 环境")) {
		t.Fatal("a correction must still reach the watch")
	}
	s.InterceptSlack("slack", correctionLine("C9", "3.0", "1.0", "0", "ai 回错了")) // someone else: not the owner's
	s.InterceptSlack("slack", correctionLine("C9", "4.0", "1.0", "1", "<@U0SELF> 帮我看下"))

	s.InterceptSlack("slack", correctionLine("C9", "2.0", "1.0", "1", "<@U0SELF> ai 回错了，应该是 sit 环境")) // redelivered
	got, err := correctionsSince(s.correctionLog(), time.Time{})
	if err != nil || len(got) != 1 || got[0].TS != "2.0" || got[0].Thread != "1.0" || !strings.Contains(got[0].Text, "sit 环境") {
		t.Fatalf("corrections = %+v, %v", got, err)
	}
	if len(f.timers) != 1 {
		t.Fatal("correction did not wake the evolution task")
	}
}

func TestCorrectionTextVariants(t *testing.T) {
	for text, want := range map[string]bool{
		"ai 回错了": true, "AI回错了，应该是 B": true, "机器人答错了": true, "这个回错了": false, "我刚才回错了": false,
		"ai 回复得很好": false, "帮我回一下": false,
	} {
		if got := correctionText.MatchString(text); got != want {
			t.Errorf("%q: got %v", text, got)
		}
	}
}

func TestRevokeTakesTheLineOutAndAllowsResuggestionLater(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	rule := "我已授权：「问进度」类事情直接回"
	if err := s.Store.AppendMemory("slack", []string{rule, "别的记忆"}, now); err != nil {
		t.Fatal(err)
	}
	accepted, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Ref: "rule:slack:问进度:as_is", Title: "规则建议", Reply: rule})
	if _, err := s.Inbox.Answer(accepted.ID, rule); err != nil {
		t.Fatal(err)
	}
	s.Inbox.SetDecision(accepted.ID, inbox.DecisionAsIs)
	s.Inbox.Close(accepted.ID, "采纳")
	if s.maySuggest("rule:slack:问进度:as_is", "slack", "问进度") {
		t.Fatal("an accepted rule must not be suggested again")
	}

	now = now.Add(time.Minute) // the revoke comes after the rule was accepted
	card, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Ref: "revoke:slack:问进度", Title: "收回授权", Reply: rule})
	if !isDecisionCard(card) {
		t.Fatal("revoke card not a decision card")
	}
	if reply := s.applySlackAnswer(card, "为什么"); !strings.Contains(reply, "只认") {
		t.Fatalf("stray text was taken: %q", reply)
	}
	closed, err := s.AnswerTodo(card.ID, TodoSend, "随便什么")
	if err != nil || closed.Status != inbox.StatusDone {
		t.Fatalf("revoke = %+v, %v", closed, err)
	}
	mem, _ := s.Store.ReadDoc("slack", "memory.md")
	if strings.Contains(mem, "问进度") || !strings.Contains(mem, "别的记忆") {
		t.Fatalf("memory = %q", mem)
	}
	if s.maySuggest("rule:slack:问进度:as_is", "slack", "问进度") {
		t.Fatal("re-suggested right after it was taken back")
	}
	now = now.Add(ruleMute + time.Hour)
	if !s.maySuggest("rule:slack:问进度:as_is", "slack", "问进度") {
		t.Fatal("never suggested again after the mute")
	}

	// A line no longer in memory reopens the card instead of silently "succeeding".
	stale, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Ref: "revoke:slack:别的", Title: "收回", Reply: "早就没了的一行"})
	if _, err := s.AnswerTodo(stale.ID, TodoSend, ""); err == nil {
		t.Fatal("removing a missing line succeeded")
	}
	if it, _ := s.Inbox.Get(stale.ID); it.Status != inbox.StatusOpen {
		t.Fatalf("stale card status = %s", it.Status)
	}
}

func TestEditedSendWakesEvolutionAndShowsInSignals(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	h := s.Handler()
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")
	it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "问数据", Title: "小林：数据？", Reply: "是 3 条", Ref: "C1:1"})
	if _, err := s.AnswerTodo(it.ID, TodoSend, "是 5 条，昨天又补了两条"); err != nil {
		t.Fatal(err)
	}
	s.Engine.Wait()
	if len(f.timers) != 1 {
		t.Fatal("an edited send did not wake the evolution task")
	}

	_, env := do(h, "GET", "/api/evolve/signals?since=2000-01-01T00:00:00Z", "", true)
	var view signalsView
	raw, _ := json.Marshal(env.Data)
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Todos) != 1 || view.Todos[0].Suggested != "是 3 条" || !strings.Contains(view.Todos[0].Answer, "5 条") {
		t.Fatalf("signals = %+v", view.Todos)
	}
}

func TestLedgerRecordsAndJudgesChanges(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	if rec, _ := do(h, "POST", "/api/evolve/ledger", `{"task":"ghost","target":"x","change":"x","why":"x","expect":"x"}`, true); rec.Code != 400 {
		t.Fatal("unknown task accepted")
	}
	rec, env := do(h, "POST", "/api/evolve/ledger", `{"task":"slack","target":"prompt.md","change":"a → b"}`, true)
	if rec.Code != 400 {
		t.Fatalf("incomplete entry accepted: %d %+v", rec.Code, env)
	}
	body := `{"task":"slack","target":"prompt.md 第 5 步","change":"a → b","why":"纠错 2 次","expect":"同类纠错归零","review_days":3}`
	_, env = do(h, "POST", "/api/evolve/ledger", body, true)
	var e LedgerEntry
	raw, _ := json.Marshal(env.Data)
	json.Unmarshal(raw, &e)
	if e.ID == "" || e.Status != LedgerWatching || e.ReviewAt.Sub(e.At) != 72*time.Hour {
		t.Fatalf("entry = %+v", e)
	}

	s.Now = func() time.Time { return e.ReviewAt.Add(time.Minute) }
	_, env = do(h, "GET", "/api/evolve/signals", "", true)
	raw, _ = json.Marshal(env.Data)
	var view signalsView
	json.Unmarshal(raw, &view)
	if len(view.LedgerDue) != 1 || view.LedgerDue[0].ID != e.ID {
		t.Fatalf("due = %+v", view.LedgerDue)
	}

	if rec, _ := do(h, "POST", "/api/evolve/ledger/"+e.ID, `{"status":"kept"}`, true); rec.Code != 400 {
		t.Fatal("verdict without evidence accepted")
	}
	if rec, _ := do(h, "POST", "/api/evolve/ledger/"+e.ID, `{"status":"kept","verdict":"三天零纠错"}`, true); rec.Code != 200 {
		t.Fatalf("verdict: %d", rec.Code)
	}
	if rec, _ := do(h, "POST", "/api/evolve/ledger/"+e.ID, `{"status":"reverted","verdict":"x"}`, true); rec.Code != 400 {
		t.Fatal("judged twice")
	}
	if rec, _ := do(h, "POST", "/api/evolve/ledger/ev-nope", `{"status":"kept","verdict":"x"}`, true); rec.Code != 404 {
		t.Fatalf("unknown id: %d", rec.Code)
	}
}
