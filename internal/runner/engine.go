// Package runner queues and executes task runs.
package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"vigil/internal/exec"
	"vigil/internal/notify"
	"vigil/internal/resident"
	"vigil/internal/result"
	"vigil/internal/store"
	"vigil/internal/task"
)

// Trigger names recorded on runs.
const (
	ByCron     = "cron"
	ByEvery    = "every"
	ByOnce     = "once"
	ByCatchUp  = "catch-up"
	ByStream   = "stream"
	ByManual   = "manual"
	ByAnswer   = "answer"
	ByRetry    = "retry"
	ByTrial    = "trial"
	ByJob      = "job"      // a background job a run handed off
	ByFeedback = "feedback" // the owner's feedback woke the self-evolution task
	ByFollow   = "follow"   // something the task was waiting for came due
)

// HeadlessRunner runs one claude -p turn.
type HeadlessRunner interface {
	Run(ctx context.Context, req exec.Request) exec.Outcome
}

// ResidentRunner runs one turn in a herdr-hosted agent.
type ResidentRunner interface {
	Turn(ctx context.Context, t task.Task, prompt string, rotate bool, reports <-chan result.Result) resident.Outcome
}

// Sender delivers notifications.
type Sender interface {
	Send(ctx context.Context, m notify.Message, custom string) error
}

// Config wires an Engine.
type Config struct {
	Store         *store.Store
	Headless      HeadlessRunner
	Resident      ResidentRunner
	Notifier      Sender
	Hub           *Hub
	VigilBin      string
	ConsoleURL    string
	MaxConcurrent int
	MaxJobs       int // background jobs running at once, across tasks (separate from MaxConcurrent)
	Logf          func(format string, args ...any)
	Now           func() time.Time
	AfterFunc     func(d time.Duration, f func()) // retry scheduling, replaceable in tests
	// Hold reports whether the owner has taken over a task; its runs are skipped meanwhile.
	Hold func(taskID string) bool
	// Idle reports how long the owner has been away from keyboard and mouse (GUI tasks wait for it).
	Idle func() (time.Duration, error)
	// Locked reports whether the screen is locked; GUI tasks cannot work then and wait.
	Locked func() (bool, error)
}

// Request asks for one run.
type Request struct {
	TaskID     string
	Trigger    string
	Input      string
	Attempt    int
	AllowTools []string  // one-shot permission grants approved by the owner
	WaitingFor time.Time // when a GUI run started waiting for the owner to step away
	// Job, when set, makes this a background job: its own lane next to the task's main runs, so a long
	// piece of work does not hold up the events behind it.
	Job      string
	JobTitle string
}

// lane is the queue a request runs in: the task itself, or one of its background jobs.
func lane(req Request) string {
	if req.Job == "" {
		return req.TaskID
	}
	return req.TaskID + "#" + req.Job
}

// Engine serialises runs per task and limits global concurrency.
type Engine struct {
	cfg  Config
	sem  chan struct{}
	jobs chan struct{} // background job slots
	ctx  context.Context
	wg   sync.WaitGroup
	mu   sync.Mutex
	qs   map[string]*queue
	live map[string]LiveRun            // lane → running run
	wait map[string]chan result.Result // run id → resident report channel
	// draining stops new runs from starting (requests stay queued) so a restart can wait for the
	// runs in progress instead of cutting them off; drainGen invalidates an older auto-expiry.
	draining bool
	holdJobs bool // only background jobs are held: an install waiting for long jobs
	drainGen int
}

// LiveRun describes a run in progress.
type LiveRun struct {
	TaskID    string    `json:"task_id,omitempty"` // filled by Running
	RunID     string    `json:"run_id"`
	Trigger   string    `json:"trigger"`
	StartedAt time.Time `json:"started_at"`
	Job       string    `json:"job,omitempty"`
	Title     string    `json:"title,omitempty"`
}

type queue struct {
	pending *Request
	running bool
}

// New creates an engine; runs stop when ctx is cancelled.
func New(ctx context.Context, cfg Config) *Engine {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = 3
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Hub == nil {
		cfg.Hub = NewHub()
	}
	return &Engine{cfg: cfg, ctx: ctx, sem: make(chan struct{}, cfg.MaxConcurrent), jobs: make(chan struct{}, cfg.MaxJobs),
		qs: map[string]*queue{}, live: map[string]LiveRun{}, wait: map[string]chan result.Result{}}
}

// Hub returns the event hub.
func (e *Engine) Hub() *Hub { return e.cfg.Hub }

// Submit enqueues a run. While a run of the same task is queued or running, requests merge:
// inputs are concatenated and the trigger of the newest request wins.
func (e *Engine) Submit(req Request) {
	key := lane(req)
	e.mu.Lock()
	q := e.qs[key]
	if q == nil {
		q = &queue{}
		e.qs[key] = q
	}
	if q.pending != nil {
		req = merge(*q.pending, req)
	}
	q.pending = &req
	start := !q.running && !e.heldLocked(*q.pending)
	if start {
		q.running = true
	}
	e.mu.Unlock()
	if start {
		e.wg.Add(1)
		go e.drain(key)
	}
}

func merge(old, next Request) Request {
	out := next
	if !isAutomatic(old.Trigger) && isAutomatic(next.Trigger) {
		out.Trigger = old.Trigger
	}
	for _, rule := range old.AllowTools {
		if !slices.Contains(out.AllowTools, rule) {
			out.AllowTools = append(out.AllowTools, rule)
		}
	}
	switch {
	case old.Input == "":
	case next.Input == "" || strings.Contains(old.Input, next.Input):
		out.Input = old.Input // a repeated submission adds nothing
	default:
		out.Input = old.Input + "\n" + next.Input
	}
	return out
}

func (e *Engine) drain(key string) {
	defer e.wg.Done()
	for {
		e.mu.Lock()
		q := e.qs[key]
		if q.pending == nil || e.ctx.Err() != nil || e.heldLocked(*q.pending) {
			q.running = false
			e.mu.Unlock()
			return
		}
		req := *q.pending
		q.pending = nil
		e.mu.Unlock()

		slots := e.sem
		if req.Job != "" {
			slots = e.jobs
		}
		select {
		case slots <- struct{}{}:
		case <-e.ctx.Done():
			e.requeue(key, req) // kept for the queue file written at shutdown
			continue
		}
		if e.held(req) {
			e.requeue(key, req) // the lane stays ours; the loop top releases it while draining
			<-slots
			continue
		}
		e.safeExecute(req)
		<-slots
	}
}

// safeExecute keeps one broken run from taking down the daemon.
func (e *Engine) safeExecute(req Request) {
	defer func() {
		if r := recover(); r != nil {
			e.setLive(lane(req), nil)
			e.cfg.Logf("run %s panicked: %v", req.TaskID, r)
		}
	}()
	e.execute(req)
}

// Wait blocks until all queued runs have finished (used at shutdown and in tests).
func (e *Engine) Wait() { e.wg.Wait() }

// Live returns each task's main run in progress, keyed by task id.
func (e *Engine) Live() map[string]LiveRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]LiveRun, len(e.live))
	for k, v := range e.live {
		if !strings.Contains(k, "#") {
			out[k] = v
		}
	}
	return out
}

// Jobs returns a task's background jobs in progress, oldest first.
func (e *Engine) Jobs(taskID string) []LiveRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []LiveRun
	for k, v := range e.live {
		if strings.HasPrefix(k, taskID+"#") {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b LiveRun) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}

// Busy reports whether a task's main lane has a run queued or running (background jobs do not count:
// they never hold up the task's own runs).
func (e *Engine) Busy(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.qs[taskID]
	return q != nil && (q.running || q.pending != nil)
}

// Active reports whether a task has anything queued or running, background jobs included.
func (e *Engine) Active(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, q := range e.qs {
		if (key == taskID || strings.HasPrefix(key, taskID+"#")) && (q.running || q.pending != nil) {
			return true
		}
	}
	return false
}

// ErrUnknownRun is returned by Report for runs that are not waiting for a report.
var ErrUnknownRun = errors.New("run is not waiting for a report")

// Report delivers a resident agent's result for a running run.
func (e *Engine) Report(runID string, r result.Result) error {
	e.mu.Lock()
	ch, ok := e.wait[runID]
	e.mu.Unlock()
	if !ok {
		return e.lateReport(runID, r)
	}
	select {
	case ch <- r:
		return nil
	default:
		return errors.New("run already reported")
	}
}

func (e *Engine) setLive(key string, lr *LiveRun) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if lr == nil {
		delete(e.live, key)
		return
	}
	e.live[key] = *lr
}

func (e *Engine) waitFor(runID string) (chan result.Result, func()) {
	ch := make(chan result.Result, 1)
	e.mu.Lock()
	e.wait[runID] = ch
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.wait, runID)
		e.mu.Unlock()
	}
}

// NewRunID returns a sortable, shell-safe run id.
func NewRunID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("r%s-%x", now.Format("20060102-150405"), b)
}
