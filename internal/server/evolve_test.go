package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/store"
)

type fakeEvolve struct {
	mu     sync.Mutex
	inputs []string
	tasks  []string // the task each round was about ("" for a round over all tasks)
	timers []func()
	delays []time.Duration
}

func (f *fakeEvolve) evolver(task string) *Evolver {
	return &Evolver{Task: task, PerDay: 2,
		Submit: func(task, in string) {
			f.mu.Lock()
			f.tasks, f.inputs = append(f.tasks, task), append(f.inputs, in)
			f.mu.Unlock()
		},
		After: func(d time.Duration, fn func()) {
			f.mu.Lock()
			f.timers, f.delays = append(f.timers, fn), append(f.delays, d)
			f.mu.Unlock()
		}}
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
	e.Wake("slack", "纠错 A")
	e.Wake("slack", "纠错 B") // same burst: no second timer
	if len(f.timers) != 1 {
		t.Fatalf("timers = %d", len(f.timers))
	}
	f.fireAll()
	if len(f.inputs) != 1 || !strings.Contains(f.inputs[0], "纠错 A") || !strings.Contains(f.inputs[0], "纠错 B") {
		t.Fatalf("inputs = %q", f.inputs)
	}
	e.Wake("slack", "C")
	f.fireAll()
	e.Wake("slack", "D") // third round today: over the cap, waits for the daily round
	f.fireAll()
	if len(f.inputs) != 2 {
		t.Fatalf("rounds = %d, want the cap of 2", len(f.inputs))
	}
}

func TestEachTaskWithFeedbackGetsItsOwnEvolutionRound(t *testing.T) {
	f := &fakeEvolve{}
	e := f.evolver("evo")
	e.PerDay = 8
	e.Wake("slack", "待办 td-1 你改了建议回复")
	e.Wake("tracker", "待办 td-2 你改成了指示")
	e.Wake("slack", "代回后对方说不对")
	e.Wake("evo", "你改了自进化提的规则卡") // about the evolution task itself: a round over everything
	if len(f.timers) != 1 {
		t.Fatalf("timers = %d, want one burst", len(f.timers))
	}
	f.fireAll()
	got := map[string]string{}
	for i, task := range f.tasks {
		got[task] = f.inputs[i]
	}
	if len(got) != 3 {
		t.Fatalf("rounds = %v", f.tasks)
	}
	if in := got["slack"]; !strings.Contains(in, "td-1") || !strings.Contains(in, "代回后") || strings.Contains(in, "td-2") ||
		!strings.Contains(in, "evolve signals --task slack") {
		t.Fatalf("slack round = %q", in)
	}
	if in := got["tracker"]; !strings.Contains(in, "td-2") || strings.Contains(in, "td-1") {
		t.Fatalf("tracker round = %q", in)
	}
	if in, ok := got[""]; !ok || !strings.Contains(in, "规则卡") {
		t.Fatalf("the evolution task's own feedback should go to a round over everything: %v", got)
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
	if err := s.Store.AppendRules("slack", []string{rule, "决策：别的记忆"}, now); err != nil {
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

func TestPickedChoiceDoesNotWakeEvolutionButTypedInstructionDoes(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")
	choices := []inbox.Choice{{Label: "代我点完并回线程", Note: "点完再回"}, {Label: "我自己点", Note: "不回"}}
	picked, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "推流程", Title: "小林：请点发布节点", Reply: "点好了", Ref: "C1:1", Choices: choices})
	if _, err := s.AnswerChoice(picked.ID, "代我点完并回线程", ""); err != nil {
		t.Fatal(err)
	}
	s.Engine.Wait()
	if len(f.timers) != 0 {
		t.Fatal("pressing one of the item's own buttons woke the evolution task")
	}
	if got, _ := s.Inbox.Get(picked.ID); got.Decision != inbox.DecisionInstructed {
		t.Fatalf("decision = %q, want it still recorded", got.Decision)
	}

	typed, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "推流程", Title: "小林：再点一个", Reply: "点好了", Ref: "C1:2", Choices: choices})
	if _, err := s.AnswerTodo(typed.ID, TodoInstruct, "先别点，等测试过了"); err != nil {
		t.Fatal(err)
	}
	s.Engine.Wait()
	if len(f.timers) != 1 {
		t.Fatal("a typed instruction instead of the suggested reply did not wake the evolution task")
	}
}

func TestSignalsFlagMemoryOverItsCap(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: big\nname: Big\ntrigger: {kind: manual}\n"), true)
	do(h, "POST", "/api/tasks", applyJSON("id: small\nname: Small\ntrigger: {kind: manual}\n"), true)
	rule := "纠错：" + strings.Repeat("长", 400)
	for range store.MemoryCap / len(rule) {
		if err := s.Store.AppendRules("big", []string{rule}, s.Now()); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Store.AppendRules("big", []string{rule, rule}, s.Now())
	_ = s.Store.AppendMemory("small", []string{"[x] " + strings.Repeat("知", 20000)}, s.Now()) // facts do not count

	_, env := do(h, "GET", "/api/evolve/signals?since=2000-01-01T00:00:00Z", "", true)
	view := decodeData[signalsView](t, env.Data)
	if len(view.MemoryOver) != 1 || view.MemoryOver[0].Task != "big" || view.MemoryOver[0].Bytes <= store.MemoryCap {
		t.Fatalf("memory_over = %+v", view.MemoryOver)
	}
}

func TestSignalsCanBeNarrowedToOneTask(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	do(h, "POST", "/api/tasks", applyJSON("id: other\nname: Other\ntrigger: {kind: manual}\n"), true)
	for _, task := range []string{"slack", "other"} {
		it, _, _ := s.Inbox.Add(inbox.Item{TaskID: task, Kind: "问数据", Title: task + "：数据？", Reply: "是 3 条", Ref: task + ":1"})
		if _, err := s.AnswerTodo(it.ID, TodoSend, "是 5 条"); err != nil {
			t.Fatal(err)
		}
		do(h, "POST", "/api/evolve/ledger", `{"task":"`+task+`","target":"m","change":"c","why":"w","expect":"e"}`, true)
	}
	s.Engine.Wait()

	if rec, _ := do(h, "GET", "/api/evolve/signals?since=2000-01-01T00:00:00Z&task=ghost", "", true); rec.Code != 400 {
		t.Fatalf("unknown task = %d", rec.Code)
	}
	_, env := do(h, "GET", "/api/evolve/signals?since=2000-01-01T00:00:00Z&task=slack", "", true)
	v := decodeData[signalsView](t, env.Data)
	if v.Task != "slack" || len(v.Todos) != 1 || v.Todos[0].Task != "slack" || len(v.Watching) != 1 || v.Watching[0].Task != "slack" {
		t.Fatalf("narrowed signals = %+v", v)
	}
	for _, d := range v.Autonomy.Days {
		if d.Task != "slack" {
			t.Fatalf("autonomy leaks another task: %+v", v.Autonomy.Days)
		}
	}
	_, env = do(h, "GET", "/api/evolve/signals?since=2000-01-01T00:00:00Z", "", true)
	if all := decodeData[signalsView](t, env.Data); len(all.Todos) != 2 || len(all.Watching) != 2 {
		t.Fatalf("unfiltered signals = %d todos, %d changes", len(all.Todos), len(all.Watching))
	}
}
