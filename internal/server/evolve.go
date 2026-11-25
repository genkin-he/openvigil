package server

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/store"
)

// Self-evolution: a task (self-evolve) reads what the owner's feedback and the runs say since it last ran,
// turns it into changes to the other tasks' rules and memory, and checks a while later whether each change
// actually helped. Vigil supplies the facts (signals), keeps the change ledger, and wakes the task soon after
// strong feedback (a correction, an edited or overruled suggestion) instead of waiting for its daily round.
const (
	evolveDelay     = 15 * time.Minute // feedback is handled this soon; a burst of it in one round
	evolvePerDay    = 8                // feedback rounds a day at most; the daily round runs regardless
	evolveReasons   = 20
	evolveLookback  = 7 * 24 * time.Hour // the first round (no earlier run) looks back this far
	signalTextLen   = 600
	signalMaxPerSet = 30
)

// Evolver wakes the self-evolution task soon after feedback, so a lesson is learned before the same mistake
// comes round again. Feedback is gathered per task: when the burst is over, each task with feedback gets a
// round of its own (only its signals, only its files), and feedback about the evolution task itself goes to
// a round over everything. Rounds per day are capped.
type Evolver struct {
	Task   string
	Delay  time.Duration
	PerDay int
	Submit func(task, input string)    // task "" is a round over all tasks
	After  func(time.Duration, func()) // default time.AfterFunc
	Now    func() time.Time
	Logf   func(string, ...any)

	mu      sync.Mutex
	pending map[string][]string // reasons by task
	armed   bool
	gen     int // the armed timer's generation; an older timer that fires does nothing
	day     string
	count   int
}

// Wake records feedback about a task; a round comes within Delay, taking whatever else arrives meanwhile.
func (e *Evolver) Wake(task, reason string) {
	if e == nil || e.Submit == nil {
		return
	}
	if task == e.Task {
		task = "" // the evolution task's own rules: a round over everything
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if day := e.now().Format("2006-01-02"); day != e.day {
		e.day, e.count = day, 0
		if !e.armed {
			e.pending = nil // yesterday's leftovers were covered by the daily round
		}
	}
	if e.pending == nil {
		e.pending = map[string][]string{}
	}
	if len(e.pending[task]) < evolveReasons {
		e.pending[task] = append(e.pending[task], clipRunes(oneLine(reason), 200))
	}
	if e.armed {
		return
	}
	if e.count >= e.perDay() {
		if e.Logf != nil {
			e.Logf("evolve: %d feedback rounds today already, %q waits for the daily round", e.count, reason)
		}
		return
	}
	e.armed = true
	e.gen++
	gen := e.gen
	after := e.After
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	after(e.delay(), func() { e.fire(gen) })
}

func (e *Evolver) fire(gen int) {
	e.mu.Lock()
	if gen != e.gen {
		e.mu.Unlock()
		return
	}
	pending := e.pending
	e.pending, e.armed = nil, false
	tasks := make([]string, 0, len(pending))
	for task := range pending {
		tasks = append(tasks, task)
	}
	sort.Strings(tasks)
	var due []string
	for _, task := range tasks {
		if e.count >= e.perDay() {
			if e.Logf != nil {
				e.Logf("evolve: daily cap reached, feedback on %q waits for the daily round", task)
			}
			continue
		}
		e.count++
		due = append(due, task)
	}
	e.mu.Unlock()
	for _, task := range due {
		e.Submit(task, evolveInput(task, pending[task]))
	}
}

// evolveInput is the input of a feedback round: one task's, or (task "") the evolution task's own.
func evolveInput(task string, reasons []string) string {
	list := "\n- " + strings.Join(reasons, "\n- ")
	if task == "" {
		return "这一轮由新反馈触发（不是每日例行轮），先处理这些，再照常看 signals：" + list
	}
	return "这一轮由新反馈触发，只进化任务「" + task + "」（单任务进化轮，见 brief「单任务进化轮」）。" +
		"先跑 $V evolve signals --task " + task + "，只读、只改这个任务的文件（~/.vigil/tasks/" + task + "/），" +
		"判断原则（owner.md）和别的任务留给例行轮。这次的反馈：" + list
}

func (e *Evolver) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Evolver) delay() time.Duration {
	if e.Delay > 0 {
		return e.Delay
	}
	return evolveDelay
}

func (e *Evolver) perDay() int {
	if e.PerDay > 0 {
		return e.PerDay
	}
	return evolvePerDay
}

// wakeEvolve passes feedback to the evolver; reasons are short, the task reads the details from signals.
func (s *Server) wakeEvolve(task, reason string) {
	if s.Evolve != nil && s.Evolve.Task != "" {
		s.Evolve.Wake(task, reason)
	}
}

// feedbackFor names what a decision on a to-do teaches, or "" when it teaches nothing new.
func feedbackFor(it inbox.Item, decision string) string {
	switch decision {
	case inbox.DecisionEdited:
		return "待办 " + it.ID + "（" + it.TaskID + "）你改了建议回复再发出：" + it.Title
	case inbox.DecisionInstructed:
		if it.Reply != "" {
			return "待办 " + it.ID + "（" + it.TaskID + "）你没用建议回复，改成了指示：" + it.Title
		}
	}
	return ""
}

type signalTodo struct {
	ID        string    `json:"id"`
	Task      string    `json:"task"`
	Kind      string    `json:"kind,omitempty"`
	Title     string    `json:"title"`
	Decision  string    `json:"decision"`
	Suggested string    `json:"suggested,omitempty"` // the agent's suggested reply
	Answer    string    `json:"answer,omitempty"`    // what the owner sent or instructed instead
	At        time.Time `json:"at"`
}

type signalRun struct {
	Task   string    `json:"task"`
	Run    string    `json:"run"`
	Status string    `json:"status"`
	Text   string    `json:"text"`
	Denied []string  `json:"denied,omitempty"`
	At     time.Time `json:"at"`
}

type signalsView struct {
	Since       time.Time    `json:"since"`
	Task        string       `json:"task,omitempty"` // set when the signals are narrowed to one task
	Corrections []Correction `json:"corrections"`
	// OwnerReplies are what the owner wrote by hand in to-dos' conversations: the draft next to the real reply.
	OwnerReplies []OwnerReply `json:"owner_replies"`
	// AfterReplies are agent replies sent with the owner's account and what the asker said next.
	AfterReplies []Aftermath   `json:"after_replies"`
	Todos        []signalTodo  `json:"todos"`
	Runs         []signalRun   `json:"runs"`
	LedgerDue    []ledgerDue   `json:"ledger_due"`
	Watching     []LedgerEntry `json:"ledger_watching"`
	// MemoryOver lists tasks whose memory.md (read every round) has outgrown store.MemoryCap.
	MemoryOver []memoryOver `json:"memory_over"`
	// Autonomy is the last week's trend: how often the owner had to step in, by day and by kind.
	Autonomy autonomyView `json:"autonomy"`
}

type memoryOver struct {
	Task  string `json:"task"`
	Bytes int64  `json:"bytes"`
	Cap   int64  `json:"cap"`
}

// oversizedMemory lists the tasks whose rules no longer fit the memory cap, largest first.
func (s *Server) oversizedMemory() []memoryOver {
	tasks, _ := s.Store.List()
	out := []memoryOver{}
	for _, t := range tasks {
		n, err := s.Store.MemorySize(t.ID)
		if err != nil {
			s.logf("memory size %s: %v", t.ID, err)
			continue
		}
		if n > store.MemoryCap {
			out = append(out, memoryOver{Task: t.ID, Bytes: n, Cap: store.MemoryCap})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// signals gathers the feedback since the self-evolution task last finished a round (or ?since=RFC3339).
func (s *Server) signals(_ http.ResponseWriter, r *http.Request) (any, error) {
	since, err := s.signalsSince(r.URL.Query().Get("since"))
	if err != nil {
		return nil, err
	}
	task := strings.TrimSpace(r.URL.Query().Get("task"))
	if task != "" {
		if _, err := s.Store.Get(task); err != nil {
			return nil, badRequest("没有任务 %s", task)
		}
	}
	return s.signalsFor(since, task)
}

// signalsFor gathers the feedback since a time, for one task or (task "") for all of them.
func (s *Server) signalsFor(since time.Time, task string) (signalsView, error) {
	mine := func(t string) bool { return task == "" || t == task }
	view := signalsView{Since: since, Task: task, LedgerDue: []ledgerDue{}, Watching: []LedgerEntry{}}
	corrections, err := correctionsSince(s.correctionLog(), since)
	if err != nil {
		return view, err
	}
	if task != "" && (s.Mirror == nil || s.Mirror.Task != task) {
		corrections = nil // corrections are said in Slack, about the task the Slack watch feeds
	}
	view.Corrections = orEmpty(clipCorrections(corrections))
	items, err := s.Inbox.List(true)
	if err != nil {
		return view, err
	}
	items = keep(items, func(it inbox.Item) bool { return mine(it.TaskID) })
	view.Todos = orEmpty(todoSignals(items, since))
	view.Autonomy = autonomyOf(items, s.Now())
	if err := s.replySignals(&view, since, mine); err != nil {
		return view, err
	}
	runs, err := s.runSignals(since)
	if err != nil {
		return view, err
	}
	view.Runs = orEmpty(keep(runs, func(r signalRun) bool { return mine(r.Task) }))
	view.MemoryOver = keep(s.oversizedMemory(), func(m memoryOver) bool { return mine(m.Task) })
	return view, s.ledgerSignals(&view, mine)
}

// replySignals adds what the owner wrote by hand and what askers said after an agent reply. A narrowed
// view reads further back before clipping, so other tasks' entries do not crowd this one's out.
func (s *Server) replySignals(view *signalsView, since time.Time, mine func(string) bool) error {
	limit := signalMaxPerSet
	if view.Task != "" {
		limit = signalMaxPerSet * 20
	}
	owner, err := ownerRepliesSince(s.ownerReplies(), since, limit)
	if err != nil {
		return err
	}
	view.OwnerReplies = orEmpty(firstN(keep(owner, func(o OwnerReply) bool { return mine(o.Task) }), signalMaxPerSet))
	after, err := aftermathSince(s.aftermathLog(), since, limit)
	if err != nil {
		return err
	}
	view.AfterReplies = orEmpty(firstN(keep(after, func(a Aftermath) bool { return mine(a.Task) }), signalMaxPerSet))
	return nil
}

// ledgerSignals splits the watched changes into due (measured since the change) and still watching.
func (s *Server) ledgerSignals(view *signalsView, mine func(string) bool) error {
	entries, err := s.ledger().list()
	if err != nil {
		return err
	}
	now := s.Now()
	for _, e := range entries {
		if e.Status != LedgerWatching || !mine(e.Task) {
			continue
		}
		if reviewDay(e.ReviewAt, now).After(now) {
			view.Watching = append(view.Watching, e)
			continue
		}
		due := ledgerDue{LedgerEntry: e}
		if e.Metric != "" {
			after, err := s.measureKind(e.Metric, e.At, now)
			if err != nil {
				return err
			}
			due.After = &after
		}
		view.LedgerDue = append(view.LedgerDue, due)
	}
	return nil
}

// keep returns the elements of xs that ok accepts, in order.
func keep[T any](xs []T, ok func(T) bool) []T {
	out := make([]T, 0, len(xs))
	for _, x := range xs {
		if ok(x) {
			out = append(out, x)
		}
	}
	return out
}

// firstN returns at most n leading elements.
func firstN[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

// orEmpty keeps an empty list a list in JSON ([] rather than null), which agents read more reliably.
func orEmpty[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func (s *Server) signalsSince(raw string) (time.Time, error) {
	if raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, badRequest("since 要用 RFC3339 时间，例如 2026-11-11T09:00:00+08:00")
		}
		return t, nil
	}
	since := s.Now().Add(-evolveLookback)
	if s.Evolve == nil || s.Evolve.Task == "" {
		return since, nil
	}
	runs, err := s.Store.Runs(s.Evolve.Task, 0)
	if err != nil {
		return since, nil // no such task yet: look back the default window
	}
	for _, r := range runs { // newest first
		if r.Status == store.StatusOK && !r.EndedAt.IsZero() {
			return r.StartedAt, nil
		}
	}
	return since, nil
}

func todoSignals(items []inbox.Item, since time.Time) []signalTodo {
	var out []signalTodo
	for _, it := range items {
		if it.Decision == "" || it.Decision == inbox.DecisionAsIs || it.Updated.Before(since) || isDecisionCard(it) {
			continue
		}
		out = append(out, signalTodo{ID: it.ID, Task: it.TaskID, Kind: it.Kind, Title: it.Title, Decision: it.Decision,
			Suggested: clipRunes(it.Reply, signalTextLen), Answer: clipRunes(it.Answer, signalTextLen), At: it.Updated})
	}
	if len(out) > signalMaxPerSet {
		out = out[len(out)-signalMaxPerSet:]
	}
	return out
}

func (s *Server) runSignals(since time.Time) ([]signalRun, error) {
	tasks, _ := s.Store.List() // a broken task file is reported elsewhere; the rest still count
	var out []signalRun
	for _, t := range tasks {
		if s.Evolve != nil && t.ID == s.Evolve.Task {
			continue // its own rounds are not feedback on the others
		}
		runs, err := s.Store.Runs(t.ID, 0)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			if r.StartedAt.Before(since) {
				break // newest first
			}
			text := ""
			switch r.Status {
			case store.StatusFail, store.StatusTimeout:
				text = firstNonEmpty(r.Error, r.Summary)
			case store.StatusNeedsYou:
				text = firstNonEmpty(r.Question, r.Summary)
			default:
				if len(r.Denied) == 0 {
					continue
				}
				text = r.Summary
			}
			out = append(out, signalRun{Task: t.ID, Run: r.ID, Status: r.Status, Text: clipRunes(oneLine(text), signalTextLen),
				Denied: r.Denied, At: r.StartedAt})
			if len(out) >= signalMaxPerSet*2 {
				return out, nil
			}
		}
	}
	return out, nil
}

// reviewDay is the start of the day a review falls on: reviews go by the day, so a round earlier that day
// (the nightly one runs seconds before the time it booked a week ago) still sees the entry due.
func reviewDay(at, now time.Time) time.Time {
	at = at.In(now.Location())
	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
}
