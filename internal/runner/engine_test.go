package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"vigil/internal/exec"
	"vigil/internal/notify"
	"vigil/internal/resident"
	"vigil/internal/result"
	"vigil/internal/store"
	"vigil/internal/stream"
	"vigil/internal/task"
)

type fakeHeadless struct {
	mu       sync.Mutex
	reqs     []exec.Request
	outcomes []exec.Outcome
	block    chan struct{}
}

func (f *fakeHeadless) Run(_ context.Context, req exec.Request) exec.Outcome {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	o := f.outcomes[0]
	if len(f.outcomes) > 1 {
		f.outcomes = f.outcomes[1:]
	}
	return o
}

type fakeResident struct {
	report bool
	rotate []bool
	out    *resident.Outcome // returned as is when set
}

func (f *fakeResident) Turn(_ context.Context, _ task.Task, _ string, rotate bool, reports <-chan result.Result) resident.Outcome {
	f.rotate = append(f.rotate, rotate)
	if f.out != nil {
		return *f.out
	}
	if f.report {
		r := <-reports
		return resident.Outcome{Result: &r, SessionID: "rs"}
	}
	return resident.Outcome{FinalText: "screen"}
}

type fakeSender struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (f *fakeSender) Send(_ context.Context, m notify.Message, _ string) error {
	f.mu.Lock()
	f.msgs = append(f.msgs, m)
	f.mu.Unlock()
	return nil
}

const okBlock = "完成\n```json\n{\"status\":\"ok\",\"summary\":\"同步了 3 条\",\"memory\":[\"上次同步到 #42\"]}\n```"

type harness struct {
	s      *store.Store
	h      *fakeHeadless
	r      *fakeResident
	n      *fakeSender
	e      *Engine
	retrys []time.Duration
}

func newHarness(t *testing.T, outcomes ...exec.Outcome) *harness {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{s: s, h: &fakeHeadless{outcomes: outcomes}, r: &fakeResident{}, n: &fakeSender{}}
	h.e = New(context.Background(), Config{Store: s, Headless: h.h, Resident: h.r, Notifier: h.n,
		AfterFunc: func(d time.Duration, _ func()) { h.retrys = append(h.retrys, d) }})
	return h
}

func (h *harness) put(t *testing.T, tk task.Task) task.Task {
	t.Helper()
	tk = task.WithDefaults(tk)
	if err := h.s.Put(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func (h *harness) run(req Request) []store.RunRecord {
	h.e.Submit(req)
	h.e.Wait()
	runs, _ := h.s.Runs(req.TaskID, 0)
	return runs
}

func manual(id string) task.Task {
	return task.Task{ID: id, Name: id, Enabled: true, Trigger: task.Trigger{Kind: task.TriggerManual}}
}

func TestHeadlessRunRecordsResultMemoryAndSession(t *testing.T) {
	h := newHarness(t, exec.Outcome{SessionID: "s1", FinalText: okBlock, CostUSD: 0.2})
	h.put(t, manual("a"))
	runs := h.run(Request{TaskID: "a", Trigger: ByManual})
	if len(runs) != 1 || runs[0].Status != store.StatusOK || runs[0].Summary != "同步了 3 条" || !runs[0].Structured {
		t.Fatalf("runs = %+v", runs)
	}
	topics, _ := h.s.Knowledge("a") // the result's memory note is a fact: it lands in the knowledge files
	st, _ := h.s.State("a")
	if len(topics) != 1 || topics[0].Entries != 1 || st.SessionID != "s1" || st.SessionRuns != 1 || st.RunCount != 1 {
		t.Fatalf("knowledge=%+v state=%+v", topics, st)
	}
}

func TestContinueResumesUntilRotation(t *testing.T) {
	h := newHarness(t, exec.Outcome{SessionID: "s1", FinalText: okBlock})
	tk := manual("a")
	tk.Session = task.Session{Mode: task.SessionContinue, RotateEvery: 2}
	h.put(t, tk)
	for range 3 {
		h.run(Request{TaskID: "a", Trigger: ByManual})
	}
	got := []string{h.h.reqs[0].ResumeSession, h.h.reqs[1].ResumeSession, h.h.reqs[2].ResumeSession}
	if got[0] != "" || got[1] != "s1" || got[2] != "" {
		t.Fatalf("resume sequence = %q", got)
	}
}

func TestFailureNotifiesRetriesAndDropsSession(t *testing.T) {
	h := newHarness(t, exec.Outcome{SessionID: "s1", Err: context.DeadlineExceeded})
	h.put(t, manual("a"))
	runs := h.run(Request{TaskID: "a", Trigger: ByManual})
	st, _ := h.s.State("a")
	if runs[0].Status != store.StatusFail || st.FailStreak != 1 || st.SessionID != "" {
		t.Fatalf("run=%+v state=%+v", runs[0], st)
	}
	if len(h.n.msgs) != 1 || !strings.Contains(h.n.msgs[0].Title, "失败") {
		t.Fatalf("notifications = %+v", h.n.msgs)
	}
	if len(h.retrys) != 1 || h.retrys[0] != 2*time.Minute {
		t.Fatalf("retry not scheduled: %v", h.retrys)
	}
}

func TestNeedsYouNotifiesWithOptions(t *testing.T) {
	text := "```json\n{\"status\":\"needs_you\",\"summary\":\"要合并吗\",\"options\":[\"A: 合并\",\"B: 不合\"],\"recommendation\":\"A\"}\n```"
	h := newHarness(t, exec.Outcome{FinalText: text})
	h.put(t, manual("a"))
	h.run(Request{TaskID: "a", Trigger: ByManual})
	if len(h.n.msgs) != 1 || !strings.Contains(h.n.msgs[0].Detail, "B: 不合") || !strings.Contains(h.n.msgs[0].Detail, "推荐：A") {
		t.Fatalf("notifications = %+v", h.n.msgs)
	}
	if len(h.retrys) != 0 {
		t.Fatal("needs_you must not retry")
	}
}

func TestSignalSkipsUnchanged(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock})
	tk := manual("a")
	tk.Signal = "echo same"
	h.put(t, tk)
	h.run(Request{TaskID: "a", Trigger: ByCron})
	runs := h.run(Request{TaskID: "a", Trigger: ByCron})
	if len(h.h.reqs) != 1 || runs[0].Status != store.StatusSkipped {
		t.Fatalf("calls=%d runs=%+v", len(h.h.reqs), runs)
	}
	h.run(Request{TaskID: "a", Trigger: ByManual})
	if len(h.h.reqs) != 2 {
		t.Fatal("manual runs must ignore the signal")
	}
}

func TestStreamBatchAckedOnlyOnSuccess(t *testing.T) {
	h := newHarness(t, exec.Outcome{Err: context.Canceled}, exec.Outcome{FinalText: okBlock})
	tk := task.Task{ID: "w", Enabled: true, Trigger: task.Trigger{Kind: task.TriggerStream, Stream: &task.Stream{Command: "true"}}}
	h.put(t, tk)
	dir, _ := h.s.SpoolDir("w")
	sp, _ := stream.OpenSpool(dir)
	_, _ = sp.Append("SLACK 你好", time.Now())

	h.run(Request{TaskID: "w", Trigger: ByStream})
	if left, _ := sp.Pending(0); len(left) != 1 {
		t.Fatalf("events lost after failure: %d", len(left))
	}
	h.run(Request{TaskID: "w", Trigger: ByStream})
	if left, _ := sp.Pending(0); len(left) != 0 {
		t.Fatalf("events not acked: %d", len(left))
	}
	if !strings.Contains(h.h.reqs[1].Prompt, "SLACK 你好") {
		t.Fatal("event not in prompt")
	}
	if runs := h.run(Request{TaskID: "w", Trigger: ByStream}); len(runs) != 2 {
		t.Fatalf("empty stream batch should not run, got %d runs", len(runs))
	}
	runs := h.run(Request{TaskID: "w", Trigger: ByManual})
	if len(runs) != 3 || !strings.Contains(runs[0].Input, "手动触发") {
		t.Fatalf("manual run of a stream task without events must still run: %d runs, input %q", len(runs), runs[0].Input)
	}
}

func TestResidentUsesReport(t *testing.T) {
	h := newHarness(t, exec.Outcome{})
	h.r.report = true
	tk := manual("r")
	tk.Mode = task.ModeResident
	h.put(t, tk)
	h.e.Submit(Request{TaskID: "r", Trigger: ByManual})
	var runID string
	for i := 0; i < 200 && runID == ""; i++ {
		time.Sleep(5 * time.Millisecond)
		runID = h.e.Live()["r"].RunID
	}
	if err := h.e.Report(runID, result.Result{Status: result.OK, Summary: "常驻完成"}); err != nil {
		t.Fatal(err)
	}
	h.e.Wait()
	runs, _ := h.s.Runs("r", 0)
	if runs[0].Summary != "常驻完成" || runs[0].SessionID != "rs" {
		t.Fatalf("runs = %+v", runs)
	}
	if err := h.e.Report(runID, result.Result{}); err == nil {
		t.Fatal("late report accepted")
	}
}

func TestSubmitMergesWhileBusy(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock})
	h.h.block = make(chan struct{})
	h.put(t, manual("a"))
	h.e.Submit(Request{TaskID: "a", Trigger: ByManual, Input: "一"})
	time.Sleep(20 * time.Millisecond)
	h.e.Submit(Request{TaskID: "a", Trigger: ByManual, Input: "二"})
	h.e.Submit(Request{TaskID: "a", Trigger: ByAnswer, Input: "三"})
	close(h.h.block)
	h.e.Wait()
	if len(h.h.reqs) != 2 || !strings.Contains(h.h.reqs[1].Prompt, "二\n三") {
		t.Fatalf("requests = %d, second prompt merged? %v", len(h.h.reqs), len(h.h.reqs) > 1 && strings.Contains(h.h.reqs[1].Prompt, "二\n三"))
	}
}

func TestRecoverInterrupted(t *testing.T) {
	h := newHarness(t, exec.Outcome{})
	h.put(t, manual("a"))
	_ = h.s.AppendRun(store.RunRecord{ID: "r1", TaskID: "a", Status: store.StatusRunning, Input: interruptedNote + "回小林"})
	redo, err := RecoverInterrupted(h.s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := h.s.Runs("a", 0)
	if runs[0].Status != store.StatusFail || len(redo) != 1 || redo[0].TaskID != "a" || redo[0].Input != interruptedNote+"回小林" {
		t.Fatalf("runs = %+v redo = %+v", runs, redo)
	}
	if again, _ := RecoverInterrupted(h.s, time.Now()); len(again) != 0 {
		t.Fatal("interrupted run must be re-run only once")
	}
}

func TestPausedTaskDropsAutomaticRunsButNotManual(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock})
	tk := manual("p")
	tk.Enabled = false
	h.put(t, tk)
	for _, by := range []string{ByCron, ByRetry, ByStream} {
		h.run(Request{TaskID: "p", Trigger: by, Input: "x"})
	}
	if len(h.h.reqs) != 0 {
		t.Fatalf("paused task ran %d automatic runs", len(h.h.reqs))
	}
	h.run(Request{TaskID: "p", Trigger: ByTrial})
	if len(h.h.reqs) != 1 {
		t.Fatal("trial run of a draft must still run")
	}
}

func TestShutdownMidRunIsRerunAtNextStart(t *testing.T) {
	s, _ := store.New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	h := &fakeHeadless{outcomes: []exec.Outcome{{Err: context.Canceled}}, block: make(chan struct{})}
	n := &fakeSender{}
	e := New(ctx, Config{Store: s, Headless: h, Notifier: n, AfterFunc: func(time.Duration, func()) { t.Fatal("no in-memory retry on shutdown") }})
	_ = s.Put(task.WithDefaults(manual("m")))
	e.Submit(Request{TaskID: "m", Trigger: ByCron, Input: "批次"})
	time.Sleep(20 * time.Millisecond)
	cancel()
	close(h.block)
	e.Wait()
	if len(n.msgs) != 0 {
		t.Fatal("shutdown must not notify")
	}
	redo, err := RecoverInterrupted(s, time.Now())
	if err != nil || len(redo) != 1 || redo[0].Input != interruptedNote+"批次" || redo[0].Trigger != ByRetry {
		t.Fatalf("redo = %+v, %v", redo, err)
	}
}

func TestDeniedOperationsBecomeADecision(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock, Denied: []string{"Bash(touch:*)"}})
	h.put(t, manual("d"))
	runs := h.run(Request{TaskID: "d", Trigger: ByManual})
	r := runs[0]
	if r.Status != store.StatusNeedsYou || !strings.Contains(r.Question, "Bash(touch:*)") || r.Recommendation != "A" || len(r.Denied) != 1 {
		t.Fatalf("run = %+v", r)
	}
	h.run(Request{TaskID: "d", Trigger: ByAnswer, AllowTools: r.Denied})
	if got := h.h.reqs[1].AllowTools; len(got) != 1 || got[0] != "Bash(touch:*)" {
		t.Fatalf("allow rules not passed: %v", got)
	}
}

func TestTheDecisionSaysWhichCommandsWereBlocked(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock, Denied: []string{"Bash(deploycli:*)"}, Blocked: []exec.Blocked{
		{Rule: "Bash(deploycli:*)", What: "看测试环境的发布状态：deploycli status --env staging app-a"},
		{Rule: "Bash(deploycli:*)", What: "deploycli rollout --env staging app-a"},
	}})
	h.put(t, manual("w"))
	r := h.run(Request{TaskID: "w", Trigger: ByManual})[0]
	for _, want := range []string{"deploycli status --env staging app-a", "deploycli rollout --env staging app-a", "看测试环境的发布状态"} {
		if !strings.Contains(r.Summary, want) {
			t.Fatalf("the card must show each blocked command, missing %q: %q", want, r.Summary)
		}
	}
}

// An operation already allowed for the task and still refused was stopped by something an allow rule cannot
// lift (the owner's own ask rules, e.g. every --env prod command): offering "allow and re-run" would ask the
// same question forever (the Slack task 11-23 17:33 / 18:18 picked "永久放行" for Bash(deploycli:*), 11-24 09:50
// was refused again) and re-run a job that already replied on Slack. The round keeps its own result and says
// what was refused.
func TestDenialsAnAllowRuleCannotLiftAreNotAsked(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock, Denied: []string{"Bash(deploycli:*)"}})
	tk := manual("g")
	tk.Agent.AllowedTools = []string{"Bash(deploycli:*)"}
	h.put(t, tk)
	r := h.run(Request{TaskID: "g", Trigger: ByManual})[0]
	if r.Status != store.StatusOK || r.Question != "" || len(r.Options) != 0 {
		t.Fatalf("a refusal no allow rule can lift must not become a decision: %+v", r)
	}
	if !strings.Contains(r.Summary, "Bash(deploycli:*)") || !strings.Contains(r.Summary, "确认规则") {
		t.Fatalf("the summary must still say what was refused and why: %q", r.Summary)
	}
	if len(r.Denied) != 1 {
		t.Fatalf("the refusal stays on the record: %+v", r.Denied)
	}
}

func TestOnlyDenialsAnAllowRuleCanLiftAreAsked(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock, Denied: []string{"Bash(deploycli:*)", "Bash(touch:*)"}})
	tk := manual("m2")
	tk.Agent.AllowedTools = []string{"Bash(deploycli:*)"}
	h.put(t, tk)
	r := h.run(Request{TaskID: "m2", Trigger: ByManual})[0]
	if r.Status != store.StatusNeedsYou || !strings.Contains(r.Question, "Bash(touch:*)") || strings.Contains(r.Question, "Bash(deploycli:*)") {
		t.Fatalf("only the liftable refusal is asked about: %+v", r)
	}
}

func TestARefusalAfterAOneOffAllowIsNotAskedAgain(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock, Denied: []string{"Bash(touch:*)"}})
	h.put(t, manual("o"))
	r := h.run(Request{TaskID: "o", Trigger: ByAnswer, AllowTools: []string{"Bash(touch:*)"}})[0]
	if r.Status != store.StatusOK || r.Question != "" {
		t.Fatalf("allowed once and refused anyway must not ask again: %+v", r)
	}
}

func TestGUITaskWaitsForOwnerToStepAway(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock})
	idleFor := time.Duration(0)
	h.e.cfg.Idle = func() (time.Duration, error) { return idleFor, nil }
	tk := manual("gui")
	tk.Hands = task.Hands{GUI: true}
	h.put(t, tk)

	h.run(Request{TaskID: "gui", Trigger: ByCron})
	if len(h.h.reqs) != 0 || len(h.retrys) != 1 || h.retrys[0] != 2*time.Minute {
		t.Fatalf("active owner: calls=%d rechecks=%v", len(h.h.reqs), h.retrys)
	}
	h.run(Request{TaskID: "gui", Trigger: ByManual})
	if len(h.h.reqs) != 1 {
		t.Fatal("manual runs must not wait")
	}
	runs := h.run(Request{TaskID: "gui", Trigger: ByCron, WaitingFor: time.Now().Add(-4 * time.Hour)})
	if runs[0].Status != store.StatusSkipped {
		t.Fatalf("waited too long should skip: %+v", runs[0])
	}
	idleFor = 10 * time.Minute
	h.run(Request{TaskID: "gui", Trigger: ByCron})
	if len(h.h.reqs) != 2 || !strings.Contains(h.h.reqs[1].Prompt, "操作电脑界面") {
		t.Fatal("away owner: GUI run should go ahead with the hands guide")
	}
}

func TestGUITaskWaitsWhileScreenLocked(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock})
	h.e.cfg.Idle = func() (time.Duration, error) { return time.Hour, nil } // away for long…
	locked := true
	h.e.cfg.Locked = func() (bool, error) { return locked, nil } // …but the screen is locked
	tk := manual("gl")
	tk.Hands = task.Hands{GUI: true}
	h.put(t, tk)
	h.run(Request{TaskID: "gl", Trigger: ByCron})
	if len(h.h.reqs) != 0 || len(h.retrys) != 1 {
		t.Fatalf("locked screen must defer: calls=%d rechecks=%d", len(h.h.reqs), len(h.retrys))
	}
	runs := h.run(Request{TaskID: "gl", Trigger: ByCron, WaitingFor: time.Now().Add(-4 * time.Hour)})
	if runs[0].Status != store.StatusSkipped || !strings.Contains(runs[0].Summary, "屏幕锁定中") {
		t.Fatalf("skip reason = %+v", runs[0])
	}
	locked = false
	h.run(Request{TaskID: "gl", Trigger: ByCron})
	if len(h.h.reqs) != 1 {
		t.Fatal("unlocked and idle: should run")
	}
}

func TestMergeDropsRepeatedInput(t *testing.T) {
	a := Request{TaskID: "x", Trigger: ByAnswer, Input: "问：范围\n答：B"}
	got := merge(merge(a, a), a)
	if got.Input != a.Input {
		t.Fatalf("repeated answers piled up: %q", got.Input)
	}
	if merged := merge(a, Request{TaskID: "x", Trigger: ByAnswer, Input: "答：直连数据库"}); !strings.Contains(merged.Input, "直连数据库") {
		t.Fatal("a different answer must still be added")
	}
}

func TestRepeatedSkipsFoldIntoOneRecord(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	held := true
	e := New(context.Background(), Config{Store: s, Headless: &fakeHeadless{outcomes: []exec.Outcome{{FinalText: okBlock}}},
		Hold: func(string) bool { return held }})
	tk := task.WithDefaults(manual("h"))
	if err := s.Put(tk); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.Submit(Request{TaskID: "h", Trigger: ByManual})
		e.Wait()
	}
	runs, _ := s.Runs("h", 0)
	if len(runs) != 1 || runs[0].Status != store.StatusSkipped || runs[0].Repeats != 3 {
		t.Fatalf("takeover skips should fold into one record: %+v", runs)
	}
	if runs[0].EndedAt.Before(runs[0].StartedAt) {
		t.Fatalf("folded record must span start to end: %+v", runs[0])
	}

	// Once the owner hands the task back, it runs again and a later skip starts a new record.
	held = false
	e.Submit(Request{TaskID: "h", Trigger: ByManual})
	e.Wait()
	held = true
	e.Submit(Request{TaskID: "h", Trigger: ByManual})
	e.Wait()
	runs, _ = s.Runs("h", 0)
	if len(runs) != 3 || runs[0].Repeats != 1 {
		t.Fatalf("a skip after a real run must not fold into the old one: %+v", runs)
	}
}

func TestResumedSessionRecordsOnlyTheRunsOwnCost(t *testing.T) {
	// claude reports a resumed session's running total; rotation starts a new total.
	h := newHarness(t,
		exec.Outcome{SessionID: "s1", FinalText: okBlock, CostUSD: 0.5},
		exec.Outcome{SessionID: "s1", FinalText: okBlock, CostUSD: 1.25},
		exec.Outcome{SessionID: "s2", FinalText: okBlock, CostUSD: 0.4})
	tk := manual("a")
	tk.Session = task.Session{Mode: task.SessionContinue, RotateEvery: 2}
	h.put(t, tk)
	var runs []store.RunRecord
	for range 3 {
		runs = h.run(Request{TaskID: "a", Trigger: ByManual})
	}
	got := map[float64]bool{}
	for _, r := range runs {
		got[r.CostUSD] = true
	}
	if len(runs) != 3 || !got[0.5] || !got[0.75] || !got[0.4] {
		t.Fatalf("costs = %+v", runs)
	}
	if st, _ := h.s.State("a"); st.SessionCost != 0.4 {
		t.Fatalf("session cost = %v", st.SessionCost)
	}
}

func TestRunCostFallsBackWhenTotalDrops(t *testing.T) {
	if got := runCost(0.3, 2); got != 0.3 {
		t.Fatalf("runCost = %v", got)
	}
}

func TestTimedOutResumeKeepsTheCostBaseline(t *testing.T) {
	h := newHarness(t,
		exec.Outcome{SessionID: "s1", FinalText: okBlock, CostUSD: 5},
		exec.Outcome{SessionID: "s1", TimedOut: true}, // killed: no cost reported
		exec.Outcome{SessionID: "s1", FinalText: okBlock, CostUSD: 5.5})
	tk := manual("a")
	tk.Session = task.Session{Mode: task.SessionContinue, RotateEvery: 5}
	h.put(t, tk)
	var runs []store.RunRecord
	for range 3 {
		runs = h.run(Request{TaskID: "a", Trigger: ByManual})
	}
	if runs[0].CostUSD != 0.5 {
		t.Fatalf("run after a timeout recorded %v, want 0.5", runs[0].CostUSD)
	}
}

func TestRunsReadTheOwnersPrinciplesOnceThereAreAny(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock})
	h.put(t, manual("a"))
	h.run(Request{TaskID: "a", Trigger: ByManual})
	if strings.Contains(h.h.reqs[0].Prompt, "owner.md") {
		t.Fatal("prompt names principles that do not exist yet")
	}
	if err := h.e.cfg.Store.AppendOwner("代回不替我许诺", time.Now()); err != nil {
		t.Fatal(err)
	}
	h.run(Request{TaskID: "a", Trigger: ByManual})
	if !strings.Contains(h.h.reqs[1].Prompt, h.e.cfg.Store.OwnerPath()) {
		t.Fatal("prompt does not point at the principles")
	}
}

func TestACommandBlockedOnlyForHowItIsWrittenIsRetriedNotAsked(t *testing.T) {
	h := newHarness(t,
		exec.Outcome{SessionID: "s1", FinalText: okBlock, Denied: []string{"Bash(cd:*)", "Bash(set:*)"}, CostUSD: 0.3},
		exec.Outcome{SessionID: "s1", FinalText: okBlock, CostUSD: 0.5})
	h.put(t, manual("d"))
	runs := h.run(Request{TaskID: "d", Trigger: ByManual})
	r := runs[0]
	if r.Status != store.StatusOK || r.Question != "" || len(r.Denied) != 0 {
		t.Fatalf("a shape-only block was put to the owner: %+v", r)
	}
	if len(h.h.reqs) != 2 {
		t.Fatalf("headless runs = %d, want one retry", len(h.h.reqs))
	}
	again := h.h.reqs[1]
	if again.ResumeSession != "s1" || !strings.Contains(again.Prompt, "Bash(cd:*)") || !strings.Contains(again.Prompt, "不要重做") {
		t.Fatalf("the retry must continue the same session and say how to write it: %+v", again)
	}
	if r.CostUSD < 0.49 || r.CostUSD > 0.51 { // the resumed session reports its total
		t.Fatalf("cost = %v", r.CostUSD)
	}
}

func TestABlockThatIsAboutWhatTheCommandDoesStillGoesToTheOwner(t *testing.T) {
	h := newHarness(t,
		exec.Outcome{SessionID: "s1", FinalText: okBlock, Denied: []string{"Bash(cd:*)", "Bash(git:*)"}},
		exec.Outcome{SessionID: "s1", FinalText: okBlock})
	h.put(t, manual("d"))
	r := h.run(Request{TaskID: "d", Trigger: ByManual})[0]
	if r.Status != store.StatusNeedsYou || len(h.h.reqs) != 1 {
		t.Fatalf("a git block was retried instead of asked: %+v (runs %d)", r, len(h.h.reqs))
	}
}

// 11-25 09:34 a chat-watch turn ended on a blocked network and was recorded ok with the screen as its
// summary: no error, so the blocked-network to-do and the re-run after recovery never happened.
func TestResidentTurnEndingOnAnAPIErrorFails(t *testing.T) {
	h := newHarness(t, exec.Outcome{})
	h.r.out = &resident.Outcome{FinalText: "  回复小王\n\n⏺ API Error: Request blocked before it reached Anthropic's API (HTTP 403) · check your VPN\n  or network and retry\n\n❯ Once fixed, resend your message\n"}
	tk := manual("r")
	tk.Mode = task.ModeResident
	h.put(t, tk)
	h.run(Request{TaskID: "r", Trigger: ByStream, Input: "回复小王"})
	runs, _ := h.s.Runs("r", 0)
	if runs[0].Status != store.StatusFail || !strings.Contains(runs[0].Error, "Request blocked before it reached Anthropic") {
		t.Fatalf("run = %s %q", runs[0].Status, runs[0].Error)
	}
	if strings.Contains(runs[0].Summary, "回复小王") {
		t.Fatalf("summary is the screen: %q", runs[0].Summary)
	}
}

func TestRetryRequestCarriesCheckProgressNote(t *testing.T) {
	rec := store.RunRecord{Status: store.StatusTimeout, Summary: "跑了 30 分钟\n没结束"}
	next := retryRequest("a", Request{TaskID: "a", Input: "发早报", Attempt: 0}, rec)
	if next.Trigger != ByRetry || next.Attempt != 1 || !strings.HasSuffix(next.Input, "）\n发早报") {
		t.Fatalf("next = %+v", next)
	}
	for _, want := range []string{retryNotePrefix, "超时：跑了 30 分钟 没结束", "第 1 次重试", "消息是否已发"} {
		if !strings.Contains(next.Input, want) {
			t.Fatalf("retry input lacks %q: %q", want, next.Input)
		}
	}
	again := retryRequest("a", next, store.RunRecord{Status: store.StatusFail, Error: "exit 1"})
	if strings.Count(again.Input, retryNotePrefix) != 1 || !strings.Contains(again.Input, "失败：exit 1") ||
		!strings.Contains(again.Input, "第 2 次重试") || !strings.HasSuffix(again.Input, "）\n发早报") {
		t.Fatalf("notes stacked across retries: %q", again.Input)
	}
}

func TestRestartAndRetryNotesDoNotStack(t *testing.T) {
	retried := retryRequest("a", Request{Input: interruptedNote + "回小林"}, store.RunRecord{Status: store.StatusFail})
	if strings.Contains(retried.Input, interruptedNote) || strings.Count(retried.Input, retryNotePrefix) != 1 ||
		!strings.HasSuffix(retried.Input, "）\n回小林") {
		t.Fatalf("retry of a recovered run: %q", retried.Input)
	}
	if got := withInterruptedNote(retried.Input); got != interruptedNote+"回小林" {
		t.Fatalf("restart of a retry: %q", got)
	}
	if got := withInterruptedNote(withInterruptedNote("回小林")); got != interruptedNote+"回小林" {
		t.Fatalf("restart note stacked: %q", got)
	}
}

func TestRetryNoteTruncatesLongReason(t *testing.T) {
	note := retryNote(store.StatusFail, strings.Repeat("错", retryWhyMax+20), 1)
	if strings.Count(note, "错") != retryWhyMax || !strings.Contains(note, "…") {
		t.Fatalf("note = %q", note)
	}
}
