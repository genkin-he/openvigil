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
		text := prompt.Build(e.promptContext(lt, st, dir, rec, input))
		out = askAboutDenials(e.dispatchHeadless(lt, store.State{}, text, dir, filepath.Join(dir, "logs", rec.ID+".jsonl"), req.AllowTools))
		out.sessionID, out.sessionRuns, out.sessionCost = st.SessionID, st.SessionRuns, st.SessionCost // the task's own session carries on untouched
	} else {
		text := prompt.Build(e.promptContext(t, st, dir, rec, input))
		out = e.dispatch(t, st, text, rec.ID, dir, req.AllowTools)
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
	return askAboutDenials(e.dispatchHeadless(t, st, text, dir, filepath.Join(dir, "logs", runID+".jsonl"), allow))
}

// DenialQuestion is the question asked when the permission system refused operations in a run.
const DenialQuestion = "这一轮有操作被权限拦下了，允许后重跑吗？"

// askAboutDenials turns refused operations into a decision for the owner: allow them once and re-run.
func askAboutDenials(out outcome) outcome {
	if len(out.denied) == 0 || out.status == store.StatusFail || out.status == store.StatusTimeout {
		return out
	}
	res := out.res
	res.Status = result.NeedsYou
	res.Question = DenialQuestion + "被拦的：" + strings.Join(out.denied, "、")
	res.Options = []string{"A: 允许一次，重跑一轮", "B: 永久放行（写进任务配置，以后不再拦），重跑一轮", "C: 不允许，这一轮就按现在的结果算"}
	res.Recommendation = "A"
	out.status, out.res = store.StatusNeedsYou, res
	return out
}

func (e *Engine) dispatchHeadless(t task.Task, st store.State, text, dir, logPath string, allow []string) outcome {
	resume := ""
	if t.Session.Mode == task.SessionContinue && st.SessionID != "" && st.SessionRuns < t.Session.RotateEvery {
		resume = st.SessionID
	}
	o := e.cfg.Headless.Run(e.ctx, exec.Request{Task: t, Prompt: text, ResumeSession: resume, LogPath: logPath, AllowTools: allow, TaskDir: dir})
	out := outcome{sessionID: o.SessionID, cost: o.CostUSD, sessionCost: o.CostUSD, sessionRuns: 1, denied: o.Denied, refused: o.Refused}
	if resume != "" {
		out.sessionRuns = st.SessionRuns + 1
		// claude reports a resumed session's total so far: the run itself cost only the difference.
		out.cost = runCost(o.CostUSD, st.SessionCost)
		if o.CostUSD == 0 {
			out.sessionCost = st.SessionCost // no report (e.g. killed on timeout): keep the baseline
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
		out.status, out.res, out.dialog = store.StatusNeedsYou, blockedResult(o.Blocked), true
	case o.TimedOut:
		out.status, out.res = store.StatusTimeout, result.Fallback(result.Fail, "超时："+o.FinalText)
	case o.Err != nil:
		out.status, out.res, out.errText = store.StatusFail, result.Fallback(result.Fail, o.Err.Error()), o.Err.Error()
	default:
		out.status, out.res = store.StatusOK, result.Fallback(result.OK, o.FinalText)
	}
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

	rec.Status, rec.Summary, rec.Structured = out.status, out.res.Summary, out.structured
	rec.Question, rec.Options, rec.Recommendation = out.res.Question, out.res.Options, out.res.Recommendation
	rec.SessionID, rec.CostUSD, rec.Error, rec.EndedAt = out.sessionID, out.cost, out.errText, now
	rec.Denied, rec.Refused, rec.Dialog = out.denied, out.refused, out.dialog
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
			crashed, hasCrashed = r.Input, true
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
	out := askAboutDenials(e.dispatchHeadless(jt, store.State{}, prompt.Build(c), dir, filepath.Join(dir, "logs", rec.ID+".jsonl"), req.AllowTools))
	e.setLive(key, nil)

	if e.ctx.Err() != nil {
		return // left as running: the next start re-runs the job
	}
	if len(out.res.Memory) > 0 {
		if err := s.AppendMemory(t.ID, out.res.Memory, e.cfg.Now()); err != nil {
			e.cfg.Logf("memory %s: %v", t.ID, err)
		}
	}
	rec.Status, rec.Summary, rec.Structured = out.status, out.res.Summary, out.structured
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
		redo = append(redo, Request{TaskID: taskID, Trigger: ByJob, Input: r.Input, Job: r.Job, JobTitle: r.JobTitle})
	}
	return redo, nil
}
