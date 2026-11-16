package server

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/store"
)

// The retro endpoint gathers a period's facts in one call, for the weekly review task to read.
const (
	retroDefaultDays = 7
	retroMaxDays     = 31
	retroCauseLen    = 80
	retroTopCauses   = 5
	retroOldTodos    = 10
)

var digits = regexp.MustCompile(`\d+`)

type retroView struct {
	From   time.Time     `json:"from"`
	To     time.Time     `json:"to"`
	Tasks  []retroTask   `json:"tasks"`
	Todos  retroTodos    `json:"todos"`
	Checks []retroCheck  `json:"checks,omitempty"`
	Self   *retroSelf    `json:"selftest,omitempty"`
	Kinds  []retroKind   `json:"kinds"`
	Slow   []retroTodoAt `json:"slowest_todos"`
	// Corrections are the replies the owner said were wrong; Changes what the self-evolution changed.
	Corrections []Correction   `json:"corrections"`
	Changes     map[string]int `json:"changes_by_status"`
}

type retroTask struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Enabled   bool           `json:"enabled"`
	Runs      int            `json:"runs"`
	ByStatus  map[string]int `json:"by_status"`
	Jobs      int            `json:"jobs"`
	Light     int            `json:"light_runs"`
	CostUSD   float64        `json:"cost_usd"`
	AvgMin    float64        `json:"avg_minutes"`
	MaxMin    float64        `json:"max_minutes"`
	Failures  []retroCause   `json:"failure_causes,omitempty"`
	NeedsYou  []retroCause   `json:"needs_you,omitempty"`
	Proposals int            `json:"pending_proposals"`
}

type retroCause struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

type retroTodos struct {
	Created     int            `json:"created"`
	Closed      int            `json:"closed"`
	Open        int            `json:"open_now"`
	ByDecision  map[string]int `json:"by_decision"`
	MedianHours float64        `json:"median_hours_to_close"`
}

type retroKind struct {
	Task       string         `json:"task"`
	Kind       string         `json:"kind"`
	Count      int            `json:"count"`
	ByDecision map[string]int `json:"by_decision"`
}

type retroTodoAt struct {
	ID     string  `json:"id"`
	Task   string  `json:"task"`
	Title  string  `json:"title"`
	Status string  `json:"status"`
	Hours  float64 `json:"hours"`
}

type retroCheck struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

type retroSelf struct {
	Status  string    `json:"status"`
	Started time.Time `json:"started_at"`
}

func (s *Server) retro(_ http.ResponseWriter, r *http.Request) (any, error) {
	days := retroDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > retroMaxDays {
			return nil, badRequest("days 要在 1～%d 之间", retroMaxDays)
		}
		days = n
	}
	to := s.Now()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	view := retroView{From: from, To: to}

	tasks, _ := s.Store.List()
	for _, t := range tasks {
		runs, err := s.Store.Runs(t.ID, 0)
		if err != nil {
			return nil, err
		}
		props, _ := s.Store.Proposals(t.ID)
		view.Tasks = append(view.Tasks, retroTaskFor(t.ID, t.Name, t.Enabled, runs, from, len(props)))
	}
	items, err := s.Inbox.List(true)
	if err != nil {
		return nil, err
	}
	view.Todos, view.Kinds, view.Slow = retroTodosFor(items, from, to)
	if s.Checks != nil {
		for name, res := range s.Checks() {
			view.Checks = append(view.Checks, retroCheck{Name: name, OK: res.OK})
		}
		sort.Slice(view.Checks, func(i, j int) bool { return view.Checks[i].Name < view.Checks[j].Name })
	}
	if view.Corrections, err = correctionsSince(s.correctionLog(), from); err != nil {
		return nil, err
	}
	view.Corrections = orEmpty(clipCorrections(view.Corrections))
	if view.Changes, err = s.ledgerCounts(from); err != nil {
		return nil, err
	}
	if s.SelfTest != nil {
		if last := s.SelfTest.Last(); last != nil {
			view.Self = &retroSelf{Status: last.Status, Started: last.Started}
		}
	}
	return view, nil
}

// ledgerCounts counts the changes made since from by their verdict.
func (s *Server) ledgerCounts(from time.Time) (map[string]int, error) {
	all, err := s.ledger().list()
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, e := range all {
		if !e.At.Before(from) || (!e.Closed.IsZero() && !e.Closed.Before(from)) {
			out[e.Status]++
		}
	}
	return out, nil
}

func retroTaskFor(id, name string, enabled bool, runs []store.RunRecord, from time.Time, proposals int) retroTask {
	rt := retroTask{ID: id, Name: name, Enabled: enabled, ByStatus: map[string]int{}, Proposals: proposals}
	fails, needs := map[string]int{}, map[string]int{}
	var total float64
	timed := 0
	for _, r := range runs {
		if r.StartedAt.Before(from) {
			continue
		}
		n := max(r.Repeats, 1)
		rt.Runs += n
		rt.ByStatus[r.Status] += n
		rt.CostUSD += r.CostUSD
		if r.Job != "" {
			rt.Jobs++
		}
		if r.Model != "" {
			rt.Light++
		}
		if !r.EndedAt.IsZero() && r.Status != store.StatusSkipped {
			m := r.EndedAt.Sub(r.StartedAt).Minutes()
			total += m
			timed++
			rt.MaxMin = max(rt.MaxMin, m)
		}
		switch r.Status {
		case store.StatusFail, store.StatusTimeout:
			fails[cause(firstNonEmpty(r.Error, r.Summary, r.Status))]++
		case store.StatusNeedsYou:
			needs[cause(firstNonEmpty(r.Question, r.Summary))]++
		}
	}
	if timed > 0 {
		rt.AvgMin = round1(total / float64(timed))
	}
	rt.MaxMin = round1(rt.MaxMin)
	rt.CostUSD = round2(rt.CostUSD)
	rt.Failures, rt.NeedsYou = topCauses(fails), topCauses(needs)
	return rt
}

func retroTodosFor(items []inbox.Item, from, to time.Time) (retroTodos, []retroKind, []retroTodoAt) {
	sum := retroTodos{ByDecision: map[string]int{}}
	kinds := map[string]*retroKind{}
	var closeHours []float64
	var slow []retroTodoAt
	for _, it := range items {
		if it.Status != inbox.StatusDone {
			sum.Open++
		}
		if it.Created.Before(from) && (it.Status != inbox.StatusDone || it.Updated.Before(from)) {
			continue
		}
		if !it.Created.Before(from) {
			sum.Created++
		}
		if it.Decision != "" {
			sum.ByDecision[it.Decision]++
		}
		end := to
		if it.Status == inbox.StatusDone {
			sum.Closed++
			end = it.Updated
			closeHours = append(closeHours, end.Sub(it.Created).Hours())
		}
		slow = append(slow, retroTodoAt{ID: it.ID, Task: it.TaskID, Title: it.Title, Status: it.Status, Hours: round1(end.Sub(it.Created).Hours())})
		if it.Kind != "" {
			key := it.TaskID + "\x00" + it.Kind
			k := kinds[key]
			if k == nil {
				k = &retroKind{Task: it.TaskID, Kind: it.Kind, ByDecision: map[string]int{}}
				kinds[key] = k
			}
			k.Count++
			if it.Decision != "" {
				k.ByDecision[it.Decision]++
			}
		}
	}
	sort.Float64s(closeHours)
	if n := len(closeHours); n > 0 {
		sum.MedianHours = round1(closeHours[n/2])
	}
	sort.Slice(slow, func(i, j int) bool { return slow[i].Hours > slow[j].Hours })
	if len(slow) > retroOldTodos {
		slow = slow[:retroOldTodos]
	}
	out := make([]retroKind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, *k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return sum, out, slow
}

// cause shortens a failure text so the same cause groups together (ids and numbers vary between runs).
func cause(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	t = digits.ReplaceAllString(t, "#")
	return clipRunes(t, retroCauseLen)
}

func topCauses(m map[string]int) []retroCause {
	out := make([]retroCause, 0, len(m))
	for t, n := range m {
		out = append(out, retroCause{Text: t, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > retroTopCauses {
		out = out[:retroTopCauses]
	}
	return out
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func round2(f float64) float64 { return math.Round(f*100) / 100 }
