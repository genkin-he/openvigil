package server

import (
	"net/http"
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
	evolveDelay     = 15 * time.Minute // feedback that comes in a burst is handled in one round
	evolvePerDay    = 4                // feedback rounds a day at most; the daily round runs regardless
	evolveReasons   = 20
	evolveLookback  = 7 * 24 * time.Hour // the first round (no earlier run) looks back this far
	signalTextLen   = 600
	signalMaxPerSet = 30
)

// Evolver wakes the self-evolution task after feedback, batching a burst and capping rounds per day.
type Evolver struct {
	Task   string
	Delay  time.Duration
	PerDay int
	Submit func(input string)
	After  func(time.Duration, func()) // default time.AfterFunc
	Now    func() time.Time
	Logf   func(string, ...any)

	mu      sync.Mutex
	pending []string
	armed   bool
	day     string
	count   int
}

// Wake records why the task should look again and makes sure a round is coming.
func (e *Evolver) Wake(reason string) {
	if e == nil || e.Submit == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if day := e.now().Format("2006-01-02"); day != e.day {
		e.day, e.count = day, 0
		if !e.armed {
			e.pending = nil // yesterday's leftovers were covered by the daily round
		}
	}
	if len(e.pending) < evolveReasons {
		e.pending = append(e.pending, clipRunes(oneLine(reason), 200))
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
	e.count++
	after := e.After
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	after(e.delay(), e.fire)
}

func (e *Evolver) fire() {
	e.mu.Lock()
	reasons := e.pending
	e.pending, e.armed = nil, false
	e.mu.Unlock()
	if len(reasons) == 0 {
		return
	}
	e.Submit("这一轮由新反馈触发（不是每日例行轮），先处理这些，再照常看 signals：\n- " + strings.Join(reasons, "\n- "))
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
func (s *Server) wakeEvolve(reason string) {
	if s.Evolve != nil && s.Evolve.Task != "" {
		s.Evolve.Wake(reason)
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
	Corrections []Correction `json:"corrections"`
	// OwnerReplies are what the owner wrote by hand in to-dos' conversations: the draft next to the real reply.
	OwnerReplies []OwnerReply `json:"owner_replies"`
	// AfterReplies are agent replies sent with the owner's account and what the asker said next.
	AfterReplies []Aftermath   `json:"after_replies"`
	Todos        []signalTodo  `json:"todos"`
	Runs         []signalRun   `json:"runs"`
	LedgerDue    []LedgerEntry `json:"ledger_due"`
	Watching     []LedgerEntry `json:"ledger_watching"`
}

// signals gathers the feedback since the self-evolution task last finished a round (or ?since=RFC3339).
func (s *Server) signals(_ http.ResponseWriter, r *http.Request) (any, error) {
	since, err := s.signalsSince(r.URL.Query().Get("since"))
	if err != nil {
		return nil, err
	}
	view := signalsView{Since: since, LedgerDue: []LedgerEntry{}, Watching: []LedgerEntry{}}
	if view.Corrections, err = correctionsSince(s.correctionLog(), since); err != nil {
		return nil, err
	}
	view.Corrections = orEmpty(clipCorrections(view.Corrections))
	items, err := s.Inbox.List(true)
	if err != nil {
		return nil, err
	}
	view.Todos = orEmpty(todoSignals(items, since))
	if view.OwnerReplies, err = ownerRepliesSince(s.ownerReplies(), since, signalMaxPerSet); err != nil {
		return nil, err
	}
	view.OwnerReplies = orEmpty(view.OwnerReplies)
	if view.AfterReplies, err = aftermathSince(s.aftermathLog(), since, signalMaxPerSet); err != nil {
		return nil, err
	}
	view.AfterReplies = orEmpty(view.AfterReplies)
	if view.Runs, err = s.runSignals(since); err != nil {
		return nil, err
	}
	view.Runs = orEmpty(view.Runs)
	entries, err := s.ledger().list()
	if err != nil {
		return nil, err
	}
	now := s.Now()
	for _, e := range entries {
		if e.Status != LedgerWatching {
			continue
		}
		if !e.ReviewAt.After(now) {
			view.LedgerDue = append(view.LedgerDue, e)
		} else {
			view.Watching = append(view.Watching, e)
		}
	}
	return view, nil
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
