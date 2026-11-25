package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"vigil/internal/exec"
	"vigil/internal/notify"
	"vigil/internal/proc"
	"vigil/internal/prompt"
	"vigil/internal/resident"
	"vigil/internal/result"
	"vigil/internal/store"
	"vigil/internal/stream"
	"vigil/internal/task"
)

const (
	signalTimeout = 60 * time.Second
	residentIdle  = 2 * time.Hour // a resident session idle this long starts fresh
	historyRuns   = 5
)

// outcome is the executor-independent result of one run.
type outcome struct {
	dialog      bool
	detour      bool               // ran beside a resident agent stuck on a dialog
	unsent      bool               // a resident round that never reached the agent: it was already stuck on a dialog
	stuck       *store.StuckDialog // on a detour: the dialog the agent was stuck on
	denied      []string
	refused     []string
	status      string
	res         result.Result
	structured  bool
	sessionID   string
	sessionRuns int
	cost        float64 // this run's own cost
	sessionCost float64 // what the session has cost so far, as claude reports it (cumulative over resumes)
	errText     string
}

func (e *Engine) execute(req Request) {
	s := e.cfg.Store
	t, err := s.Get(req.TaskID)
	if err != nil {
		e.cfg.Logf("run %s: %v", req.TaskID, err)
		return
	}
	st, err := s.State(t.ID)
	if err != nil {
		e.cfg.Logf("run %s: %v", t.ID, err)
		return
	}
	dir, _ := s.TaskDir(t.ID)
	if req.Job != "" {
		e.executeJob(t, st, dir, req)
		return
	}
	if !t.Enabled && isAutomatic(req.Trigger) {
		e.cfg.Logf("%s is paused, dropping %s run", t.ID, req.Trigger)
		return
	}
	if e.ownerBusy(t, req) {
		return
	}
	if t.Mode == task.ModeHeadless && e.cfg.Hold != nil && e.cfg.Hold(t.ID) {
		e.recordSkipped(t, req, "你正在接管这个任务，本轮跳过")
		return
	}

	signal, skip := e.checkSignal(t, st, req)
	if skip {
		e.recordSkipped(t, req, "信号没有变化，跳过")
		return
	}
	batch, input, ok := e.collectInput(t, req)
	if !ok {
		return
	}

	now := e.cfg.Now()
	rec := store.RunRecord{ID: NewRunID(now), TaskID: t.ID, Trigger: req.Trigger, Status: store.StatusRunning, Input: input, StartedAt: now}
	lt, light := lightTask(t, req, batch)
	if light {
		rec.Model = lt.Agent.Model
	}
	if err := s.AppendRun(rec); err != nil {
		e.cfg.Logf("run %s: record start: %v", t.ID, err)
	}
	e.setLive(t.ID, &LiveRun{RunID: rec.ID, Trigger: req.Trigger, StartedAt: now})
	e.cfg.Hub.Publish(Event{Type: "run.started", TaskID: t.ID, RunID: rec.ID, Status: store.StatusRunning})

	var out outcome
	if light {
		out = e.runAside(lt, st, dir, rec, input, req.AllowTools)
	} else {
		text := prompt.Build(e.promptContext(t, st, dir, rec, input))
		out = e.dispatch(t, st, text, rec.ID, dir, req.AllowTools)
		if out.unsent {
			stuck := &store.StuckDialog{Summary: out.res.Summary, Question: out.res.Question, Options: out.res.Options}
			out = e.detour(t, st, dir, rec, input, req.AllowTools)
			out.stuck = stuck
		}
	}

	e.setLive(t.ID, nil)
	e.finish(t, req, rec, out, batch, signal)
}

// lightTask returns the task as the light lane runs it, and whether this batch qualifies: a plain
// stream batch (nothing merged in from the owner) whose every event matches a light pattern.
func lightTask(t task.Task, req Request, batch []stream.Event) (task.Task, bool) {
	st := t.Trigger.Stream
	if st == nil || st.Light == nil || req.Trigger != ByStream || strings.TrimSpace(req.Input) != "" || len(batch) == 0 {
		return t, false
	}
	match, err := task.IgnoreMatcher(st.Light.Match)
	if err != nil {
		return t, false
	}
	for _, ev := range batch {
		if !match(ev.Line) {
			return t, false
		}
	}
	lt := t
	lt.Mode = task.ModeHeadless
	lt.Session.Mode = task.SessionFresh
	lt.Agent.Model = st.Light.Model
	return lt, true
}

const (
	guiRecheck = 2 * time.Minute
	guiMaxWait = 3 * time.Hour
)

// ownerBusy defers an automatic GUI run while the owner is at the computer. It returns true when the
// run was postponed or given up.
func (e *Engine) ownerBusy(t task.Task, req Request) bool {
	if !t.Hands.GUI || !isAutomatic(req.Trigger) || e.cfg.Idle == nil {
		return false
	}
	reason := ""
	if e.cfg.Locked != nil {
		if locked, err := e.cfg.Locked(); err == nil && locked {
			reason = "屏幕锁定中"
		}
	}
	if reason == "" {
		idle, err := e.cfg.Idle()
		if err != nil {
			e.cfg.Logf("%s: idle time unknown (%v), running anyway", t.ID, err)
			return false
		}
		if idle >= t.Hands.RequireIdle.Std() {
			return false
		}
		reason = "你在用电脑（空闲 " + idle.Round(time.Second).String() + "）"
	}
	now := e.cfg.Now()
	if req.WaitingFor.IsZero() {
		req.WaitingFor = now
	}
	if now.Sub(req.WaitingFor) >= guiMaxWait {
		e.recordSkipped(t, req, "这一轮要操作界面，等了 3 小时一直不满足条件（"+reason+"），跳过")
		return true
	}
	e.cfg.Logf("%s: GUI run waits: %s", t.ID, reason)
	next := req
	e.cfg.AfterFunc(guiRecheck, func() {
		if e.ctx.Err() == nil {
			e.Submit(next)
		}
	})
	return true
}

// isAutomatic reports whether a trigger came from Vigil itself rather than from the owner.
func isAutomatic(trigger string) bool {
	switch trigger {
	case ByManual, ByTrial, ByAnswer:
		return false
	}
	return true
}

func isScheduled(trigger string) bool {
	return trigger == ByCron || trigger == ByEvery || trigger == ByCatchUp
}

// checkSignal runs the task's signal command for scheduled runs; skip is true when its output did not change.
func (e *Engine) checkSignal(t task.Task, st store.State, req Request) (string, bool) {
	if strings.TrimSpace(t.Signal) == "" || !isScheduled(req.Trigger) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(e.ctx, signalTimeout)
	defer cancel()
	cmd := osexec.CommandContext(ctx, "/bin/bash", "-c", t.Signal)
	cmd.Dir = exec.ExpandHome(t.Agent.Cwd)
	proc.Prepare(cmd)
	out, err := cmd.Output()
	if err != nil {
		e.cfg.Logf("signal %s failed, running anyway: %v", t.ID, err)
		return "", false
	}
	sum := sha256.Sum256(out)
	hash := hex.EncodeToString(sum[:])
	return hash, hash == st.LastSignal
}

// recordSkipped notes a skipped run. A skip for the same reason as the run just before it extends that
// record (end time and count) instead of adding another, so a takeover reads as one entry, not hundreds.
func (e *Engine) recordSkipped(t task.Task, req Request, why string) {
	now := e.cfg.Now()
	rec := store.RunRecord{ID: NewRunID(now), TaskID: t.ID, Trigger: req.Trigger, Status: store.StatusSkipped,
		Summary: why, Input: req.Input, StartedAt: now, EndedAt: now, Repeats: 1}
	if last, err := e.cfg.Store.Runs(t.ID, 1); err == nil && len(last) == 1 &&
		last[0].Status == store.StatusSkipped && last[0].Summary == why {
		rec = last[0]
		rec.EndedAt = now
		rec.Repeats = max(rec.Repeats, 1) + 1
	}
	if err := e.cfg.Store.AppendRun(rec); err != nil {
		e.cfg.Logf("record skipped %s: %v", t.ID, err)
	}
	e.cfg.Hub.Publish(Event{Type: "run.finished", TaskID: t.ID, RunID: rec.ID, Status: rec.Status})
}

// collectInput merges the request input with pending stream events. ok is false when a stream run has nothing to do.
func (e *Engine) collectInput(t task.Task, req Request) ([]stream.Event, string, bool) {
	if t.Trigger.Kind != task.TriggerStream {
		return nil, req.Input, true
	}
	spool, err := e.openSpool(t.ID)
	if err != nil {
		e.cfg.Logf("spool %s: %v", t.ID, err)
		return nil, req.Input, req.Input != ""
	}
	batch, err := spool.Pending(t.Trigger.Stream.MaxBatch)
	if err != nil {
		e.cfg.Logf("spool %s: %v", t.ID, err)
	}
	lines := make([]string, 0, len(batch)+1)
	if req.Input != "" {
		lines = append(lines, req.Input)
	}
	for _, ev := range batch {
		lines = append(lines, ev.At.Local().Format("15:04:05")+" "+ev.Line)
	}
	if len(lines) == 0 && !isAutomatic(req.Trigger) {
		// The owner asked for a run: go ahead with the routine pass even without new events.
		lines = append(lines, "手动触发：没有新事件，按 brief 做一次例行的全量处理")
	}
	return batch, strings.Join(lines, "\n"), len(lines) > 0
}

func (e *Engine) openSpool(id string) (*stream.Spool, error) {
	dir, err := e.cfg.Store.SpoolDir(id)
	if err != nil {
		return nil, err
	}
	return stream.OpenSpool(dir)
}

func (e *Engine) promptContext(t task.Task, st store.State, dir string, rec store.RunRecord, input string) prompt.Context {
	c := prompt.Context{Task: t, Dir: dir, RunID: rec.ID, RunNumber: st.RunCount + 1, Trigger: rec.Trigger, Input: input, VigilBin: e.cfg.VigilBin}
	if owner, err := e.cfg.Store.ReadOwner(); err == nil && strings.TrimSpace(owner) != "" {
		c.OwnerDoc = e.cfg.Store.OwnerPath()
	}
	if topics, err := e.cfg.Store.Knowledge(t.ID); err == nil {
		for _, k := range topics {
			c.Knowledge = append(c.Knowledge, prompt.Topic{Name: k.Topic, Entries: k.Entries, Bytes: k.Bytes})
		}
	}
	runs, _ := e.cfg.Store.Runs(t.ID, historyRuns)
	failing := true
	for _, r := range runs {
		if r.Status == store.StatusSkipped || r.Status == store.StatusRunning {
			continue
		}
		if c.LastSummary == "" {
			c.LastSummary = r.Summary
		}
		if failing && (r.Status == store.StatusFail || r.Status == store.StatusTimeout) {
			c.Failures = append(c.Failures, r.Summary)
		} else {
			failing = false
		}
	}
	return c
}

func (e *Engine) dispatch(t task.Task, st store.State, text, runID, dir string, allow []string) outcome {
	if t.Mode == task.ModeResident {
		return e.dispatchResident(t, st, text, runID)
	}
	return e.headless(t, st, text, dir, filepath.Join(dir, "logs", runID+".jsonl"), allow)
}

// DenialQuestion is the question asked when the permission system refused operations in a run.
const DenialQuestion = "这一轮有操作被权限拦下了，允许后重跑吗？"

// unliftableNote is added to a round's summary for operations refused although they were already allowed.
const unliftableNote = "被你自己的确认规则拦下、放行也绕不过的操作（这一轮按现有结果算）："

// askAboutDenials turns refused operations into a decision for the owner: allow them once and re-run.
// Operations refused although granted (the task's allowed tools or this run's one-off grants) were stopped
// by something no allow rule lifts, such as the owner's own ask rules for prod commands in a session with no
// one to answer: asking again would offer a re-run that is refused the same way, so they are only reported.
func askAboutDenials(out outcome, granted []string) outcome {
	if len(out.denied) == 0 || out.status == store.StatusFail || out.status == store.StatusTimeout {
		return out
	}
	var liftable, unliftable []string
	for _, d := range out.denied {
		if slices.Contains(granted, d) {
			unliftable = append(unliftable, d)
		} else {
			liftable = append(liftable, d)
		}
	}
	res := out.res
	if len(unliftable) > 0 {
		res.Summary = strings.TrimSpace(res.Summary + "\n" + unliftableNote + strings.Join(unliftable, "、"))
	}
	if len(liftable) > 0 {
		res.Status = result.NeedsYou
		res.Question = DenialQuestion + "被拦的：" + strings.Join(liftable, "、")
		res.Options = []string{"A: 允许一次，重跑一轮", "B: 永久放行（写进任务配置，以后不再拦），重跑一轮", "C: 不允许，这一轮就按现在的结果算"}
		res.Recommendation = "A"
		out.status = store.StatusNeedsYou
	}
	out.res = res
	return out
}

func (e *Engine) dispatchHeadless(t task.Task, st store.State, text, dir, logPath string, allow []string) outcome {
	if t.Session.Mode == task.SessionContinue && st.SessionID != "" && st.SessionRuns < t.Session.RotateEvery {
		return e.headlessOnce(t, text, st, dir, logPath, allow)
	}
	return e.headlessOnce(t, text, store.State{}, dir, logPath, allow)
}

// headlessOnce runs one headless turn, resuming prev.SessionID when it is set.
func (e *Engine) headlessOnce(t task.Task, text string, prev store.State, dir, logPath string, allow []string) outcome {
	resume := prev.SessionID
	o := e.cfg.Headless.Run(e.ctx, exec.Request{Task: t, Prompt: text, ResumeSession: resume, LogPath: logPath, AllowTools: allow, TaskDir: dir})
	out := outcome{sessionID: o.SessionID, cost: o.CostUSD, sessionCost: o.CostUSD, sessionRuns: 1, denied: o.Denied, refused: o.Refused}
	if resume != "" {
		out.sessionRuns = prev.SessionRuns + 1
		// claude reports a resumed session's total so far: the run itself cost only the difference.
		out.cost = runCost(o.CostUSD, prev.SessionCost)
		if o.CostUSD == 0 {
			out.sessionCost = prev.SessionCost // no report (e.g. killed on timeout): keep the baseline
		}
	}
	switch {
	case o.TimedOut:
		out.status, out.res = store.StatusTimeout, result.Fallback(result.Fail, "超时："+o.FinalText)
	case o.Err != nil:
		out.status, out.res, out.errText = store.StatusFail, result.Fallback(result.Fail, o.Err.Error()), o.Err.Error()
		out.sessionID, out.sessionCost = "", 0 // a broken session must not be resumed
	default:
		out.status, out.res, out.structured = classify(o.FinalText, o.IsError)
	}
	return out
}

// shapePrefixes are commands a permission rule blocks only for how a command line is written: rules match a
// command by its start, so a line opening with cd / set / export … (or chaining several commands) misses the
// rule that allows what it actually runs. Rewriting the line is enough; nothing needs the owner's say.
var shapePrefixes = map[string]bool{"cd": true, "set": true, "export": true, "source": true, "for": true,
	"while": true, "if": true, "pushd": true, "eval": true}

// shapeOnly reports whether every blocked rule (e.g. "Bash(cd:*)") is about how a command was written.
func shapeOnly(denied []string) bool {
	if len(denied) == 0 {
		return false
	}
	for _, d := range denied {
		inner, ok := strings.CutPrefix(d, "Bash(")
		name, _, _ := strings.Cut(strings.TrimSuffix(inner, ")"), ":")
		if !ok || !shapePrefixes[strings.TrimSpace(name)] {
			return false
		}
	}
	return true
}

// reshapeGuide tells the resumed session why it was stopped and how to write the command instead.
func reshapeGuide(denied []string) string {
	return "你上一步有命令被权限规则拦下了（被拦的：" + strings.Join(denied, "、") + "）。这是写法问题，不是权限不够：" +
		"规则按命令开头匹配，以 cd / set / export 等开头、或把几条命令用 && ; | 串成一行的，都对不上放行规则。\n" +
		"改成：每条命令单独执行；不要 cd，用绝对路径、git -C <仓库>、make -C <目录>；不要先 set / export 变量，把值直接写进命令。\n" +
		"从被拦的那一步接着做完。已经做过的（发过的消息、建过的待办、提交过的代码）不要重做。做完照原来的格式汇报。"
}

// reshape gives a run that was blocked only for how its commands were written one more turn in the same
// session, told how to write them, before anything goes to the owner. Same permissions, same rules: only
// the command lines change.
func (e *Engine) reshape(t task.Task, out outcome, dir, logPath string, allow []string) outcome {
	if !shapeOnly(out.denied) || out.status == store.StatusFail || out.status == store.StatusTimeout || out.sessionID == "" {
		return out
	}
	prev := store.State{SessionID: out.sessionID, SessionCost: out.sessionCost, SessionRuns: out.sessionRuns}
	again := e.headlessOnce(t, reshapeGuide(out.denied), prev, dir, logPath, allow)
	again.cost += out.cost
	return again
}

// headless runs a headless turn, retries a shape-only block once, and puts any block left to the owner.
func (e *Engine) headless(t task.Task, st store.State, text, dir, logPath string, allow []string) outcome {
	out := e.reshape(t, e.dispatchHeadless(t, st, text, dir, logPath, allow), dir, logPath, allow)
	return askAboutDenials(out, append(slices.Clone(t.Agent.AllowedTools), allow...))
}

// runCost is one run's cost given the session totals before and after it. A total lower than before means
// the report did not cover the earlier turns (an unknown baseline), so the reported figure is taken as is.
func runCost(total, before float64) float64 {
	if total < before {
		return total
	}
	return total - before
}

func classify(text string, isError bool) (string, result.Result, bool) {
	if r, ok := result.FromText(text); ok {
		return r.Status, r, true
	}
	if isError {
		return store.StatusFail, result.Fallback(result.Fail, text), false
	}
	return store.StatusOK, result.Fallback(result.OK, text), false
}

func (e *Engine) dispatchResident(t task.Task, st store.State, text, runID string) outcome {
	rotate := st.SessionRuns >= t.Session.RotateEvery ||
		(t.Session.Mode == task.SessionFresh && st.RunCount > 0) ||
		(!st.LastRunAt.IsZero() && e.cfg.Now().Sub(st.LastRunAt) > residentIdle)
	ch, done := e.waitFor(runID)
	defer done()
	o := e.cfg.Resident.Turn(e.ctx, t, text, rotate, ch)
	out := outcome{sessionID: o.SessionID, sessionRuns: st.SessionRuns + 1}
	if rotate {
		out.sessionRuns = 1
	}
	switch {
	case o.Result != nil:
		out.status, out.res, out.structured = o.Result.Status, *o.Result, true
	case o.Blocked != "":
		out.status, out.res, out.dialog, out.unsent = store.StatusNeedsYou, blockedResult(o.Blocked), true, o.Unsent
	case o.TimedOut:
		out.status, out.res = store.StatusTimeout, result.Fallback(result.Fail, "超时："+o.FinalText)
	case o.Err != nil:
		out.status, out.res, out.errText = store.StatusFail, result.Fallback(result.Fail, o.Err.Error()), o.Err.Error()
	case resident.TurnError(o.FinalText) != "":
		// The turn died on an API error (11-25: a blocked network) and the agent sits idle; as a failure it
		// raises the blocked-network to-do and is re-run once the network is back, like a headless run.
		msg := resident.TurnError(o.FinalText)
		out.status, out.res, out.errText = store.StatusFail, result.Fallback(result.Fail, msg), msg
	default:
		out.status, out.res = store.StatusOK, result.Fallback(result.OK, o.FinalText)
	}
	return out
}

// runAside runs a round in a fresh headless session beside the task's own one, which carries on untouched.
func (e *Engine) runAside(t task.Task, st store.State, dir string, rec store.RunRecord, input string, allow []string) outcome {
	text := prompt.Build(e.promptContext(t, st, dir, rec, input))
	out := e.headless(t, store.State{}, text, dir, filepath.Join(dir, "logs", rec.ID+".jsonl"), allow)
	out.sessionID, out.sessionRuns, out.sessionCost = st.SessionID, st.SessionRuns, st.SessionCost
	return out
}

// DetourNote heads the summary of a round that ran beside a resident agent stuck on a dialog.
const DetourNote = "（常驻会话停在确认框上，这一轮改在临时会话里跑了）"

// detour runs a resident round the agent never got, because it was already stuck on a dialog, in a fresh
// headless session instead of leaving it queued behind the dialog. The dialog itself was asked about by
// the round that was running when it came up; the agent picks that up again once it is answered.
func (e *Engine) detour(t task.Task, st store.State, dir string, rec store.RunRecord, input string, allow []string) outcome {
	ht := t
	ht.Mode = task.ModeHeadless
	ht.Session.Mode = task.SessionFresh
	e.cfg.Logf("%s: resident agent is stuck on a dialog, running %s in a fresh session", t.ID, rec.ID)
	out := e.runAside(ht, st, dir, rec, input, allow)
	out.res.Summary = DetourNote + "\n" + out.res.Summary
	out.detour = true
	return out
}

// blockedResult asks the owner about a dialog Vigil does not answer itself. The dialog, not the whole
// screen, is shown; its answers become the options, and picking one presses it in the agent's session.
func blockedResult(screen string) result.Result {
	p := resident.ReadPrompt(screen)
	q := "agent 停在一个确认框上，去 herdr 的 vigil 会话里处理"
	if p.Question != "" {
		q = "agent 在等你回答「" + p.Question + "」"
	}
	return result.Result{Status: result.NeedsYou, Summary: p.Body(), Question: q, Options: p.Answers()}
}

func (e *Engine) finish(t task.Task, req Request, rec store.RunRecord, out outcome, batch []stream.Event, signal string) {
	s := e.cfg.Store
	now := e.cfg.Now()
	succeeded := out.status == store.StatusOK || out.status == store.StatusNeedsYou
	if succeeded && len(batch) > 0 {
		if spool, err := e.openSpool(t.ID); err == nil {
			if err := spool.Ack(batch[len(batch)-1].Seq); err != nil {
				e.cfg.Logf("ack %s: %v", t.ID, err)
			}
		}
	}
	if len(out.res.Memory) > 0 {
		if err := s.AppendMemory(t.ID, out.res.Memory, now); err != nil {
			e.cfg.Logf("memory %s: %v", t.ID, err)
		}
	}
	shuttingDown := e.ctx.Err() != nil
	if shuttingDown {
		out.status = store.StatusFail
		out.res = result.Result{Status: result.Fail, Summary: "守护进程重启，这一轮被中断，启动后会自动补跑"}
	}
	st, err := s.UpdateState(t.ID, func(st store.State) store.State {
		if shuttingDown {
			input := req.Input
			st.Interrupted = &input
		}
		st.RunCount++
		st.LastRunAt = now
		st.SessionID = out.sessionID
		st.SessionRuns = out.sessionRuns
		st.SessionCost = out.sessionCost
		if succeeded {
			st.FailStreak = 0
			if signal != "" {
				st.LastSignal = signal
			}
		} else {
			st.FailStreak++
		}
		return st
	})
	if err != nil {
		e.cfg.Logf("state %s: %v", t.ID, err)
	}

	rec.Status, rec.Summary, rec.Structured, rec.Made = out.status, out.res.Summary, out.structured, out.res.Made
	rec.Question, rec.Options, rec.Recommendation = out.res.Question, out.res.Options, out.res.Recommendation
	rec.SessionID, rec.CostUSD, rec.Error, rec.EndedAt = out.sessionID, out.cost, out.errText, now
	rec.Denied, rec.Refused, rec.Dialog, rec.Detour = out.denied, out.refused, out.dialog, out.detour
	rec.StuckOn = out.stuck
	if err := s.AppendRun(rec); err != nil {
		e.cfg.Logf("run %s: record end: %v", t.ID, err)
	}
	e.cfg.Hub.Publish(Event{Type: "run.finished", TaskID: t.ID, RunID: rec.ID, Status: rec.Status, Text: rec.Summary})
	if shuttingDown {
		return // no notification, no in-memory retry: the next start re-runs it
	}
	e.notify(t, rec, st.FailStreak)
	if !succeeded && req.Attempt < t.Run.Retry.Max {
		next := Request{TaskID: t.ID, Trigger: ByRetry, Input: req.Input, Attempt: req.Attempt + 1}
		e.cfg.AfterFunc(t.Run.Retry.Backoff.Std(), func() {
			if e.ctx.Err() == nil {
				e.Submit(next)
			}
		})
	}
}

func (e *Engine) notify(t task.Task, rec store.RunRecord, failStreak int) {
	kind := ""
	switch rec.Status {
	case store.StatusNeedsYou:
		kind = "needs_you"
	case store.StatusFail, store.StatusTimeout:
		if notify.ShouldNotifyFailure(failStreak) {
			kind = "fail"
		}
	}
	if kind == "" || !wants(t.Notify.On, kind) || e.cfg.Notifier == nil {
		return
	}
	title := fmt.Sprintf("Vigil · %s：%s", t.Name, statusLabel(rec.Status))
	if kind == "fail" && failStreak > 1 {
		title += fmt.Sprintf("（连续 %d 次）", failStreak)
	}
	detail := rec.Question
	if len(rec.Options) > 0 {
		detail += "\n" + strings.Join(rec.Options, "\n")
	}
	if rec.Recommendation != "" {
		detail += "\n推荐：" + rec.Recommendation
	}
	msg := notify.Message{TaskID: t.ID, Title: title, Status: rec.Status, Summary: rec.Summary,
		Detail: strings.TrimSpace(detail), Link: e.cfg.ConsoleURL + "/#/task/" + t.ID}
	if err := e.cfg.Notifier.Send(e.ctx, msg, t.Notify.Command); err != nil {
		e.cfg.Logf("notify %s: %v", t.ID, err)
	}
}

func wants(on []string, kind string) bool {
	alt := strings.ReplaceAll(kind, "_", "-")
	return slices.Contains(on, kind) || slices.Contains(on, alt)
}

func statusLabel(s string) string {
	switch s {
	case store.StatusNeedsYou:
		return "需要你"
	case store.StatusTimeout:
		return "超时"
	case store.StatusFail:
		return "失败"
	}
	return s
}

// interruptedNote heads the input of a run re-run after a restart cut it off: the cut run may already
// have done part of the work, and doing it twice (a second MR, a second publish, a message sent again)
// is worse than not at all.
const interruptedNote = "（上一次跑到一半被守护进程重启打断了，这是重跑。先查清楚已经做到哪一步：MR 是否已提、发布是否已发、消息是否已发、待办是否已建，从断点继续，别重复做。）\n"

func withInterruptedNote(input string) string {
	if strings.HasPrefix(input, interruptedNote) {
		return input
	}
	return interruptedNote + input
}

// RecoverInterrupted marks runs left "running" by a previous daemon as failed and returns the requests
// to re-run: tasks whose run was cut short by a shutdown or crash. Call once at startup.
func RecoverInterrupted(s *store.Store, now time.Time) ([]Request, error) {
	tasks, _ := s.List()
	var redo []Request
	for _, t := range tasks {
		runs, err := s.Runs(t.ID, 3)
		if err != nil {
			return redo, err
		}
		crashed := ""
		hasCrashed := false
		for _, r := range runs {
			if r.Status != store.StatusRunning || r.Job != "" {
				continue // background jobs are recovered by recoverJobs
			}
			r.Status, r.Summary, r.EndedAt = store.StatusFail, "守护进程重启，这一轮被中断，启动后会自动补跑", now
			if err := s.AppendRun(r); err != nil {
				return redo, err
			}
			crashed, hasCrashed = withInterruptedNote(r.Input), true
		}
		st, err := s.UpdateState(t.ID, func(st store.State) store.State {
			if st.Interrupted == nil && hasCrashed {
				st.Interrupted = &crashed
			}
			return st
		})
		if err != nil {
			return redo, err
		}
		if st.Interrupted != nil && t.Enabled {
			redo = append(redo, Request{TaskID: t.ID, Trigger: ByRetry, Input: *st.Interrupted})
		}
		jobs, err := recoverJobs(s, t.ID, now)
		if err != nil {
			return redo, err
		}
		redo = append(redo, jobs...)
		if _, err := s.UpdateState(t.ID, func(st store.State) store.State { st.Interrupted = nil; return st }); err != nil {
			return redo, err
		}
	}
	return redo, nil
}

// jobTimeout is the least time a background job gets: jobs exist for the long work a round hands off.
const jobTimeout = 2 * time.Hour

// executeJob runs a background job: always a fresh headless session (never the task's own session, which
// the main lane keeps using), only the input it was given, no stream events, no signal check, no takeover hold.
func (e *Engine) executeJob(t task.Task, st store.State, dir string, req Request) {
	s := e.cfg.Store
	now := e.cfg.Now()
	rec := store.RunRecord{ID: NewRunID(now), TaskID: t.ID, Trigger: ByJob, Status: store.StatusRunning,
		Input: req.Input, StartedAt: now, Job: req.Job, JobTitle: req.JobTitle}
	if err := s.AppendRun(rec); err != nil {
		e.cfg.Logf("job %s: record start: %v", req.Job, err)
	}
	key := lane(req)
	e.setLive(key, &LiveRun{RunID: rec.ID, Trigger: ByJob, StartedAt: now, Job: req.Job, Title: req.JobTitle})
	e.cfg.Hub.Publish(Event{Type: "run.started", TaskID: t.ID, RunID: rec.ID, Status: store.StatusRunning, Text: req.JobTitle})

	jt := t
	jt.Mode = task.ModeHeadless
	jt.Session.Mode = task.SessionFresh
	jt.Run.Timeout = task.Duration(max(t.Run.Timeout.Std(), jobTimeout))
	c := e.promptContext(jt, st, dir, rec, req.Input)
	c.Job = req.JobTitle
	out := e.headless(jt, store.State{}, prompt.Build(c), dir, filepath.Join(dir, "logs", rec.ID+".jsonl"), req.AllowTools)
	e.setLive(key, nil)

	if e.ctx.Err() != nil {
		return // left as running: the next start re-runs the job
	}
	if len(out.res.Memory) > 0 {
		if err := s.AppendMemory(t.ID, out.res.Memory, e.cfg.Now()); err != nil {
			e.cfg.Logf("memory %s: %v", t.ID, err)
		}
	}
	rec.Status, rec.Summary, rec.Structured, rec.Made = out.status, out.res.Summary, out.structured, out.res.Made
	rec.Question, rec.Options, rec.Recommendation = out.res.Question, out.res.Options, out.res.Recommendation
	rec.SessionID, rec.CostUSD, rec.Error, rec.EndedAt = out.sessionID, out.cost, out.errText, e.cfg.Now()
	rec.Denied, rec.Refused, rec.Dialog = out.denied, out.refused, out.dialog
	if err := s.AppendRun(rec); err != nil {
		e.cfg.Logf("job %s: record end: %v", req.Job, err)
	}
	e.cfg.Hub.Publish(Event{Type: "run.finished", TaskID: t.ID, RunID: rec.ID, Status: rec.Status, Text: rec.Summary})
	e.notify(t, rec, 1)
}

// jobScan is how far back recovery looks for background jobs a restart cut off.
const jobScan = 100

// recoverJobs closes background job runs a restart left running and asks for them to run again.
func recoverJobs(s *store.Store, taskID string, now time.Time) ([]Request, error) {
	runs, err := s.Runs(taskID, jobScan)
	if err != nil {
		return nil, err
	}
	var redo []Request
	for _, r := range runs {
		if r.Job == "" || r.Status != store.StatusRunning {
			continue
		}
		r.Status, r.Summary, r.EndedAt = store.StatusFail, "守护进程重启，后台子任务被中断，已自动重跑", now
		if err := s.AppendRun(r); err != nil {
			return redo, err
		}
		redo = append(redo, Request{TaskID: taskID, Trigger: ByJob, Input: withInterruptedNote(r.Input), Job: r.Job, JobTitle: r.JobTitle})
	}
	return redo, nil
}
