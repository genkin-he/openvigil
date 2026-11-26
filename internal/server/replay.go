package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/replay"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// Replaying a rule change: past rounds of a task are put to a model with no tools, once under the current
// rules and once under the changed ones, and the two decisions are set against what the owner actually
// did. Nothing is executed; see package replay.
const (
	replayMaxRuns     = 20
	replayParallel    = 4
	replayDecidedSpan = 30 * 24 * time.Hour // to-dos the owner reworked, looked at first
	replayRecentSpan  = 7 * 24 * time.Hour  // other rounds, sampled to catch side effects
	replayDir         = "replays"
)

var replayDocs = []string{"brief.md", "runbook.md", "memory.md"}

// replayOwnerDoc is the owner's principles: not a task's file, but every round reads it, so a replay can
// change it too (judged on the rounds of req.Task) and always carries it in the rules.
const replayOwnerDoc = "owner.md"

// ReplayRequest names a rule change and, optionally, the rounds to replay it on.
type ReplayRequest struct {
	Task   string   `json:"task"`
	Doc    string   `json:"doc"`    // brief.md, runbook.md, memory.md or owner.md
	Before string   `json:"before"` // the passage as it stands; "" appends After at the end of Doc
	After  string   `json:"after"`
	Runs   []string `json:"runs,omitempty"` // rounds to replay; empty picks them
	Kind   string   `json:"kind,omitempty"` // to-do kind the change is about, for picking rounds
}

// ReplayRow is one replayed round.
type ReplayRow struct {
	RunID             string          `json:"run_id"`
	Input             string          `json:"input"`
	Before            replay.Decision `json:"before"`
	After             replay.Decision `json:"after"`
	Actual            replay.Decision `json:"actual"`
	Verdict           replay.Verdict  `json:"verdict"`
	ActualFromSummary bool            `json:"actual_from_summary,omitempty"` // no to-do: inferred from the round's summary
	NoEvidence        bool            `json:"no_evidence,omitempty"`
}

// ReplayReport is the outcome of one replay.
type ReplayReport struct {
	ID      string         `json:"id"`
	Task    string         `json:"task"`
	Doc     string         `json:"doc"`
	At      time.Time      `json:"at"`
	Status  string         `json:"status"` // running, done, failed
	Error   string         `json:"error,omitempty"`
	Rows    []ReplayRow    `json:"rows"`
	Same    int            `json:"same"`
	Closer  int            `json:"closer"`
	Farther int            `json:"farther"`
	Changed int            `json:"changed"`
	Unknown int            `json:"unknown"`
	CostUSD float64        `json:"cost_usd"`
	Summary string         `json:"summary"`
	Request *ReplayRequest `json:"request,omitempty"`
}

// Replay runs a replay to the end and saves its report.
func (s *Server) Replay(ctx context.Context, req ReplayRequest) (ReplayReport, error) {
	before, after, err := s.replayRules(req)
	if err != nil {
		return ReplayReport{}, err
	}
	rep := ReplayReport{ID: "rp-" + randHex(4), Task: req.Task, Doc: req.Doc, At: s.Now(), Status: "running", Request: &req}
	return s.runReplay(ctx, rep, req, before, after)
}

func (s *Server) runReplay(ctx context.Context, rep ReplayReport, req ReplayRequest, before, after string) (ReplayReport, error) {
	items, err := s.Inbox.List(true)
	if err != nil {
		return rep, err
	}
	runs, err := s.replayRuns(req, items)
	if err != nil {
		return rep, err
	}
	owners := s.todosByRun(req.Task, items)
	rows := make([]ReplayRow, len(runs))
	costs := make([]float64, len(runs))
	sem := make(chan struct{}, replayParallel)
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i], costs[i] = s.replayRound(ctx, req.Task, run, owners[run.ID], before, after)
		}()
	}
	wg.Wait()
	rep.Rows = rows
	if err := ctx.Err(); err != nil { // cut off (the daemon stopping): the rows say nothing
		rep.Status, rep.Error = "failed", "回放被中断："+err.Error()
		_ = s.saveReplay(rep)
		return rep, fmt.Errorf("replay %s interrupted: %w", rep.ID, err)
	}
	for i, r := range rows {
		rep.CostUSD += costs[i]
		switch r.Verdict {
		case replay.Same:
			rep.Same++
		case replay.Closer:
			rep.Closer++
		case replay.Farther:
			rep.Farther++
		case replay.Changed:
			rep.Changed++
		default:
			rep.Unknown++
		}
	}
	rep.Status = "done"
	rep.Summary = fmt.Sprintf("回放 %d 轮，%d 轮的处理会变；%d 轮更接近你当时的做法，%d 轮更远；%d 轮没法判断；花费 $%.2f",
		len(rows), rep.Closer+rep.Farther+rep.Changed, rep.Closer, rep.Farther, rep.Unknown, rep.CostUSD)
	return rep, s.saveReplay(rep)
}

// replayRound replays one round under both versions of the rules.
func (s *Server) replayRound(ctx context.Context, taskID string, run store.RunRecord, owner *inbox.Item, before, after string) (ReplayRow, float64) {
	c := replay.Case{RunID: run.ID, Input: run.Input, Evidence: s.runEvidence(taskID, run)}
	row := ReplayRow{RunID: run.ID, Input: clipRunes(run.Input, 600), NoEvidence: c.Evidence == ""}
	var cost float64
	b, cb, errB := s.Replayer.Decide(ctx, before, c)
	a, ca, errA := s.Replayer.Decide(ctx, after, c)
	cost = cb + ca
	if errB != nil || errA != nil {
		s.logf("replay %s: %v %v", run.ID, errB, errA)
	}
	row.Before, row.After = b, a
	if owner != nil {
		row.Actual = actualFromTodo(*owner)
	} else {
		row.Actual, row.ActualFromSummary = actualFromSummary(run.Summary), true
	}
	row.Verdict = replay.Compare(b, a, row.Actual)
	return row, cost
}

// replayRules returns the task's full rules as they stand and with the change applied.
func (s *Server) replayRules(req ReplayRequest) (string, string, error) {
	if _, err := s.Store.Get(req.Task); err != nil {
		return "", "", badRequest("%v", err)
	}
	if req.Doc != replayOwnerDoc && !slices.Contains(replayDocs, req.Doc) {
		return "", "", badRequest("只能回放 brief.md、runbook.md、memory.md、owner.md 的改动")
	}
	if strings.TrimSpace(req.After) == "" && req.Before == "" {
		return "", "", badRequest("after 不能为空")
	}
	docs := map[string]string{}
	for _, d := range replayDocs {
		text, _ := s.Store.ReadDoc(req.Task, d)
		docs[d] = text
	}
	owner, err := s.Store.ReadOwner()
	if err != nil {
		return "", "", err
	}
	docs[replayOwnerDoc] = owner
	changed := docs[req.Doc]
	switch {
	case req.Before == "":
		changed = strings.TrimRight(changed, "\n") + "\n" + req.After + "\n"
	case strings.Contains(changed, req.Before):
		changed = strings.Replace(changed, req.Before, req.After, 1)
	default:
		return "", "", badRequest("before 不在 %s 的当前内容里，回放不了", req.Doc)
	}
	before := rulesText(docs)
	docs[req.Doc] = changed
	return before, rulesText(docs), nil
}

var briefRef = regexp.MustCompile(`~/[^\s"'()（）]+\.md`)

// rulesText joins a task's rules, the owner's principles every round reads, and the prompt files its
// brief points at.
func rulesText(docs map[string]string) string {
	var b strings.Builder
	for _, d := range replayDocs {
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", d, docs[d])
	}
	if owner := docs[replayOwnerDoc]; strings.TrimSpace(owner) != "" {
		fmt.Fprintf(&b, "## %s（我的判断原则，所有任务共用）\n\n%s\n\n", replayOwnerDoc, owner)
	}
	home, _ := os.UserHomeDir()
	for _, ref := range briefRef.FindAllString(docs["brief.md"], -1) {
		if text, err := os.ReadFile(filepath.Join(home, strings.TrimPrefix(ref, "~/"))); err == nil {
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", ref, text)
		}
	}
	return b.String()
}

// replayRuns picks the rounds: those named, else rounds whose to-dos the owner reworked, rounds of the
// same kind, then a sample of recent ones; at most replayMaxRuns.
func (s *Server) replayRuns(req ReplayRequest, items []inbox.Item) ([]store.RunRecord, error) {
	all, err := s.Store.Runs(req.Task, 0)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.RunRecord{}
	for _, r := range all {
		// A round that carries out the owner's answer has his decision as input, not something that came in.
		if r.Status != store.StatusRunning && r.Input != "" && r.Trigger != runner.ByAnswer {
			byID[r.ID] = r
		}
	}
	var picked []store.RunRecord
	seen := map[string]bool{}
	add := func(id string) {
		if r, ok := byID[id]; ok && !seen[id] && len(picked) < replayMaxRuns {
			seen[id] = true
			picked = append(picked, r)
		}
	}
	if len(req.Runs) > 0 {
		for _, id := range req.Runs {
			add(id)
		}
		return picked, nil
	}
	now := s.Now()
	reworked := func(it inbox.Item) bool {
		return it.Decision == inbox.DecisionEdited || it.Decision == inbox.DecisionInstructed
	}
	order := []func(inbox.Item) bool{reworked}
	if req.Kind != "" { // a change about one kind is judged on that kind first, reworked ones leading
		order = []func(inbox.Item) bool{
			func(it inbox.Item) bool { return it.Kind == req.Kind && reworked(it) },
			func(it inbox.Item) bool { return it.Kind == req.Kind },
		}
	}
	for _, want := range order {
		for _, it := range items {
			if it.TaskID != req.Task || now.Sub(it.Updated) > replayDecidedSpan || !want(it) {
				continue
			}
			if r, ok := s.runOf(it); ok {
				add(r.ID)
			}
		}
	}
	var recent []string
	for _, r := range all {
		if r.Status == store.StatusOK && now.Sub(r.StartedAt) <= replayRecentSpan {
			recent = append(recent, r.ID)
		}
	}
	rand.Shuffle(len(recent), func(i, j int) { recent[i], recent[j] = recent[j], recent[i] })
	for _, id := range recent {
		add(id)
	}
	return picked, nil
}

// todosByRun maps a task's settled to-dos to the rounds that raised them.
func (s *Server) todosByRun(taskID string, items []inbox.Item) map[string]*inbox.Item {
	out := map[string]*inbox.Item{}
	for i := range items {
		it := &items[i]
		if it.TaskID != taskID || it.Decision == "" || isDecisionCard(*it) {
			continue
		}
		if r, ok := s.runOf(*it); ok {
			out[r.ID] = it
		}
	}
	return out
}

// actualFromTodo is what the owner did with a to-do, as a decision.
func actualFromTodo(it inbox.Item) replay.Decision {
	d := replay.Decision{Valid: true}
	switch it.Decision {
	case inbox.DecisionAsIs, inbox.DecisionEdited:
		d.Action, d.Text = replay.ActionReply, it.Answer
	case inbox.DecisionInstructed, inbox.DecisionHandledElsewhere:
		d.Action = replay.ActionTodo // handled elsewhere: it needed the owner, he just did it himself
	case inbox.DecisionNoise:
		d.Action = replay.ActionIgnore
	default: // stale, a reasonless dismissal: says nothing about what should have happened
		return replay.Decision{}
	}
	return d
}

// actualFromSummary infers what a round without a to-do did from its summary.
func actualFromSummary(summary string) replay.Decision {
	d := replay.Decision{Valid: true}
	switch {
	case strings.Contains(summary, "待办"):
		d.Action = replay.ActionTodo
	case strings.Contains(summary, "已回") || strings.Contains(summary, "代回") || strings.Contains(summary, "回复"):
		d.Action = replay.ActionReply
	case strings.Contains(summary, "表情") || strings.Contains(summary, "点了"):
		d.Action = replay.ActionReact
	default:
		d.Action = replay.ActionIgnore
	}
	return d
}

func (s *Server) replayPath(id string) (string, error) {
	if !strings.HasPrefix(id, "rp-") || strings.ContainsAny(id, "/\\.") {
		return "", badRequest("invalid replay id")
	}
	return filepath.Join(s.Store.Root(), replayDir, id+".json"), nil
}

func (s *Server) saveReplay(rep ReplayReport) error {
	path, err := s.replayPath(rep.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("replay dir: %w", err)
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (s *Server) loadReplay(id string) (ReplayReport, error) {
	path, err := s.replayPath(id)
	if err != nil {
		return ReplayReport{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ReplayReport{}, notFound("没有这次回放：%s", id)
	}
	var rep ReplayReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return ReplayReport{}, fmt.Errorf("read replay %s: %w", id, err)
	}
	return rep, nil
}

// replayStart begins a replay in the background (it can take minutes) and returns its id at once.
func (s *Server) replayStart(_ http.ResponseWriter, r *http.Request) (any, error) {
	var req ReplayRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	before, after, err := s.replayRules(req)
	if err != nil {
		return nil, err
	}
	rep := ReplayReport{ID: "rp-" + randHex(4), Task: req.Task, Doc: req.Doc, At: s.Now(), Status: "running", Request: &req}
	if err := s.saveReplay(rep); err != nil {
		return nil, err
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		if got, err := s.runReplay(s.lifetime(), rep, req, before, after); err != nil && got.Status != "failed" {
			rep.Status, rep.Error = "failed", err.Error()
			_ = s.saveReplay(rep)
		}
	}()
	return map[string]string{"id": rep.ID, "status": rep.Status}, nil
}

func (s *Server) replayGet(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.loadReplay(r.PathValue("id"))
}

// lifetime is the daemon's context, or a background one where none was set (tests).
func (s *Server) lifetime() context.Context {
	if s.Base != nil {
		return s.Base
	}
	return context.Background()
}
