// Package store keeps all Vigil state as plain files under one root (default ~/.vigil).
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"vigil/internal/task"
)

// Run statuses.
const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusOK       = "ok"
	StatusNeedsYou = "needs_you"
	StatusFail     = "fail"
	StatusTimeout  = "timeout"
	StatusSkipped  = "skipped"
)

const (
	fileTask    = "task.yaml"
	fileBrief   = "brief.md"
	fileRunbook = "runbook.md"
	fileMemory  = "memory.md"
	fileRuns    = "runs.jsonl"
	fileState   = "state.json"
	dirHistory  = "history"
	dirProposal = "proposals"
	dirSpool    = "spool"
	maxRunLine  = 1 << 20
)

var datePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\b`)

// ErrNotFound is returned when a task does not exist.
var ErrNotFound = errors.New("task not found")

// StuckDialog is a dialog a detour found the resident agent stopped on, as a dialog round would record it.
type StuckDialog struct {
	Summary  string   `json:"summary"`
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// RunRecord is one run of a task, one JSON line in runs.jsonl.
type RunRecord struct {
	ID             string       `json:"id"`
	TaskID         string       `json:"task_id"`
	Trigger        string       `json:"trigger"`
	Status         string       `json:"status"`
	Summary        string       `json:"summary,omitempty"`
	Question       string       `json:"question,omitempty"`
	Options        []string     `json:"options,omitempty"`
	Recommendation string       `json:"recommendation,omitempty"`
	Dialog         bool         `json:"dialog,omitempty"`   // stopped on a dialog in the agent's session; Options answer it there
	Detour         bool         `json:"detour,omitempty"`   // ran in a fresh session because the resident agent was stuck on a dialog
	StuckOn        *StuckDialog `json:"stuck_on,omitempty"` // on a detour: the dialog the resident agent was stuck on
	Denied         []string     `json:"denied,omitempty"`   // allow rules for operations the permission rules refused
	Refused        []string     `json:"refused,omitempty"`  // operations the auto mode classifier refused ("tool：reason")
	Structured     bool         `json:"structured"`
	SessionID      string       `json:"session_id,omitempty"`
	Input          string       `json:"input,omitempty"`
	Job            string       `json:"job,omitempty"`       // background job id, when this run was handed off by another run
	JobTitle       string       `json:"job_title,omitempty"` // what the job is about, one line
	Repeats        int          `json:"repeats,omitempty"`   // a skip that kept happening for the same reason, folded into one record
	Model          string       `json:"model,omitempty"`     // set when the run used a model other than the task's (the light lane)
	Error          string       `json:"error,omitempty"`
	CostUSD        float64      `json:"cost_usd,omitempty"`
	Made           []string     `json:"made,omitempty"` // what the run newly produced (an MR, a comment…), as reported
	StartedAt      time.Time    `json:"started_at"`
	EndedAt        time.Time    `json:"ended_at,omitzero"`
}

// State is the mutable runtime state of a task, kept in state.json.
type State struct {
	SessionID       string    `json:"session_id,omitempty"`
	SessionRuns     int       `json:"session_runs"`
	SessionCost     float64   `json:"session_cost_usd,omitempty"` // the session's reported total so far
	LastSignal      string    `json:"last_signal,omitempty"`
	LastRunAt       time.Time `json:"last_run_at,omitzero"`
	LastScheduledAt time.Time `json:"last_scheduled_at,omitzero"`
	FailStreak      int       `json:"fail_streak"`
	RunCount        int       `json:"run_count"`
	OnceDone        bool      `json:"once_done,omitempty"`
	AnsweredRun     string    `json:"answered_run,omitempty"`  // the needs_you run whose question was last answered
	AnsweredText    string    `json:"answered_text,omitempty"` // what the owner answered to it
	AnsweredAt      time.Time `json:"answered_at,omitzero"`    // when a dialog was last pressed from Vigil, or found already gone
	// Interrupted holds the input of a run cut short by a daemon shutdown; it is re-run at the next start.
	Interrupted *string `json:"interrupted,omitempty"`
}

// Store reads and writes the Vigil directory tree. Safe for concurrent use.
type Store struct {
	root string
	mu   sync.Mutex
}

// New returns a store rooted at root, creating the tasks directory.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o700); err != nil {
		return nil, fmt.Errorf("create store root: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) Root() string { return s.root }

// TaskDir returns the directory of a task. The id is validated to prevent path traversal.
func (s *Store) TaskDir(id string) (string, error) {
	if !task.ValidID(id) {
		return "", fmt.Errorf("invalid task id %q", id)
	}
	return filepath.Join(s.root, "tasks", id), nil
}

func (s *Store) taskFile(id, name string) (string, error) {
	dir, err := s.TaskDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// List returns all tasks sorted by id. Broken task files are reported in errs, not fatal.
func (s *Store) List() ([]task.Task, []error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "tasks"))
	if err != nil {
		return nil, []error{fmt.Errorf("list tasks: %w", err)}
	}
	var tasks []task.Task
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !task.ValidID(e.Name()) {
			continue
		}
		t, err := s.Get(e.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks, errs
}

// Get loads one task.
func (s *Store) Get(id string) (task.Task, error) {
	path, err := s.taskFile(id, fileTask)
	if err != nil {
		return task.Task{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return task.Task{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("read task %s: %w", id, err)
	}
	t, err := task.Parse(data)
	if err != nil {
		return task.Task{}, fmt.Errorf("task %s: %w", id, err)
	}
	if t.ID != id {
		return task.Task{}, fmt.Errorf("task %s: id in file is %q", id, t.ID)
	}
	return t, nil
}

// Put validates and saves a task.
func (s *Store) Put(t task.Task) error {
	t = task.WithDefaults(t)
	if err := task.Validate(t); err != nil {
		return err
	}
	data, err := task.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode task: %w", err)
	}
	dir, err := s.TaskDir(t.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create task dir: %w", err)
	}
	return writeAtomic(filepath.Join(dir, fileTask), data)
}

// Remove deletes a task directory.
func (s *Store) Remove(id string) error {
	dir, err := s.TaskDir(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return os.RemoveAll(dir)
}

// Doc names for ReadDoc / WriteDoc.
const (
	DocBrief   = fileBrief
	DocRunbook = fileRunbook
	DocMemory  = fileMemory
)

func validDoc(name string) bool {
	return name == DocBrief || name == DocRunbook || name == DocMemory
}

// ReadDoc returns brief.md, runbook.md or memory.md ("" when missing).
func (s *Store) ReadDoc(id, name string) (string, error) {
	if !validDoc(name) {
		return "", fmt.Errorf("unknown doc %q", name)
	}
	path, err := s.taskFile(id, name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", name, err)
	}
	return string(data), nil
}

// WriteDoc replaces a doc. Replacing the runbook keeps the previous version under history/.
func (s *Store) WriteDoc(id, name, content string) error {
	if !validDoc(name) {
		return fmt.Errorf("unknown doc %q", name)
	}
	path, err := s.taskFile(id, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create task dir: %w", err)
	}
	if name == DocRunbook {
		if err := s.archive(id, path); err != nil {
			return err
		}
	}
	return writeAtomic(path, []byte(content))
}

func (s *Store) archive(id, path string) error {
	old, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || len(old) == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read runbook: %w", err)
	}
	dir, err := s.taskFile(id, dirHistory)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create history dir: %w", err)
	}
	name := "runbook." + time.Now().UTC().Format("20060102T150405.000000000") + ".md"
	return writeAtomic(filepath.Join(dir, name), old)
}

// AppendMemory files an agent's notes: rules go to memory.md, facts to knowledge/<topic>.md ("[topic] …"
// picks the topic; untagged facts go to misc). Old routine notes still in memory.md move to the archive.
func (s *Store) AppendMemory(id string, notes []string, at time.Time) error {
	return s.appendNotes(id, notes, at, false)
}

// AppendRules adds notes that are rules whatever their wording (a decision, a rule the owner accepted)
// to memory.md.
func (s *Store) AppendRules(id string, notes []string, at time.Time) error {
	return s.appendNotes(id, notes, at, true)
}

func (s *Store) appendNotes(id string, notes []string, at time.Time, rules bool) error {
	var b strings.Builder
	var facts []inNote
	for _, raw := range notes {
		n, ok := splitNote(raw, at, rules)
		switch {
		case !ok:
		case n.topic == "":
			fmt.Fprintf(&b, "- %s %s\n", n.date, n.text)
		default:
			facts = append(facts, n)
		}
	}
	if b.Len() == 0 && len(facts) == 0 {
		return nil
	}
	path, err := s.taskFile(id, fileMemory)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := appendKnowledge(filepath.Join(filepath.Dir(path), KnowledgeDir), facts); err != nil {
		return err
	}
	if b.Len() > 0 {
		if err := appendFile(path, []byte(b.String())); err != nil {
			return err
		}
	}
	return archiveOldMemory(path, at) // routine notes written before facts had their own place
}

// SaveProposal stores a proposed runbook awaiting approval and returns its name.
func (s *Store) SaveProposal(id, content, reason string, at time.Time) (string, error) {
	dir, err := s.taskFile(id, dirProposal)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create proposals dir: %w", err)
	}
	name := at.UTC().Format(proposalLayout)
	body := "<!-- reason: " + strings.ReplaceAll(reason, "-->", "") + " -->\n" + content
	return name, writeAtomic(filepath.Join(dir, name+".md"), []byte(body))
}

// proposalLayout names proposal files; only names in this form are proposals, so a draft
// an agent leaves in proposals/ can never be approved over the runbook.
const proposalLayout = "20060102T150405.000000000"

func isProposalName(name string) bool {
	_, err := time.Parse(proposalLayout, name)
	return err == nil
}

// Proposal is a pending runbook change.
type Proposal struct {
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Content string `json:"content"`
}

// Proposals lists pending runbook proposals, oldest first.
func (s *Store) Proposals(id string) ([]Proposal, error) {
	dir, err := s.taskFile(id, dirProposal)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	var out []Proposal
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || !isProposalName(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read proposal: %w", err)
		}
		reason, content := splitReason(string(data))
		out = append(out, Proposal{Name: name, Reason: reason, Content: content})
	}
	return out, nil
}

func splitReason(body string) (string, string) {
	const prefix = "<!-- reason: "
	if !strings.HasPrefix(body, prefix) {
		return "", body
	}
	end := strings.Index(body, " -->\n")
	if end < 0 {
		return "", body
	}
	return body[len(prefix):end], body[end+len(" -->\n"):]
}

// ResolveProposal applies (approve=true) or discards a proposal.
func (s *Store) ResolveProposal(id, name string, approve bool) error {
	if !isProposalName(name) {
		return fmt.Errorf("invalid proposal name %q", name)
	}
	dir, err := s.taskFile(id, dirProposal)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read proposal: %w", err)
	}
	if approve {
		_, content := splitReason(string(data))
		if err := s.WriteDoc(id, DocRunbook, content); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// AppendRun appends a run record (a later record with the same id supersedes the earlier one).
func (s *Store) AppendRun(r RunRecord) error {
	path, err := s.taskFile(r.TaskID, fileRuns)
	if err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode run: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendFile(path, append(data, '\n'))
}

// Runs returns the latest version of each run, newest first, at most limit (0 = all).
func (s *Store) Runs(id string, limit int) ([]RunRecord, error) {
	path, err := s.taskFile(id, fileRuns)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open runs: %w", err)
	}
	defer f.Close()
	byID := map[string]int{}
	var runs []RunRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxRunLine)
	for sc.Scan() {
		var r RunRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if i, ok := byID[r.ID]; ok {
			runs[i] = r
			continue
		}
		byID[r.ID] = len(runs)
		runs = append(runs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read runs: %w", err)
	}
	out := make([]RunRecord, 0, len(runs))
	for i := len(runs) - 1; i >= 0; i-- {
		out = append(out, runs[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// State loads state.json (zero value when missing).
func (s *Store) State(id string) (State, error) {
	path, err := s.taskFile(id, fileState)
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	return st, nil
}

// UpdateState applies fn to the current state and saves the result atomically.
func (s *Store) UpdateState(id string, fn func(State) State) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.State(id)
	if err != nil {
		return State{}, err
	}
	next := fn(cur)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return State{}, fmt.Errorf("encode state: %w", err)
	}
	path, err := s.taskFile(id, fileState)
	if err != nil {
		return State{}, err
	}
	return next, writeAtomic(path, data)
}

// SpoolDir returns the event spool directory of a task, creating it.
func (s *Store) SpoolDir(id string) (string, error) {
	dir, err := s.taskFile(id, dirSpool)
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// ErrNoMemoryLine is returned when a note to remove is not in the task's memory.
var ErrNoMemoryLine = errors.New("memory has no such line")

// memoryNote is a memory line's note: without the leading dash and date, whitespace collapsed.
func memoryNote(line string) string {
	body := strings.TrimSpace(line)
	body = strings.TrimSpace(strings.TrimPrefix(body, "-"))
	body = datePrefix.ReplaceAllString(body, "")
	return strings.Join(strings.Fields(body), " ")
}

// RemoveMemory deletes the memory entries (bullet lines) whose note is exactly note — given with or
// without its dash and date — and returns how many went (identical duplicates all go). Nothing matching
// is ErrNoMemoryLine, so a stale request is never silent and a vague one never deletes other lines.
func (s *Store) RemoveMemory(id, note string) (int, error) {
	note = memoryNote(note)
	if note == "" {
		return 0, ErrNoMemoryLine
	}
	path, err := s.taskFile(id, fileMemory)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("read memory: %w", err)
	}
	var kept []string
	removed := 0
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") && memoryNote(line) == note {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return 0, ErrNoMemoryLine
	}
	return removed, writeAtomic(path, []byte(strings.Join(kept, "")))
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

func appendFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("append %s: %w", filepath.Base(path), err)
	}
	return f.Close()
}
