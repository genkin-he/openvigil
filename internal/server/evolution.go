package server

import (
	"sort"

	"vigil/internal/inbox"
	"vigil/internal/store"
	"vigil/internal/task"
)

// A task's evolution, shown on its page: how it may change (its evolve policy), what has been changed in
// it and with what verdict, and — for a change aimed at a kind of to-do — how that kind has gone since.
const (
	evolutionShown = 50
	roundsShown    = 10
)

type evolutionChange struct {
	LedgerEntry
	Since *autonomyCount `json:"since,omitempty"` // the metric's kind, from the change until now
}

type evolutionView struct {
	Policy  string            `json:"policy"`
	Rounds  []store.RunRecord `json:"rounds"` // the self-evolution task's rounds about this task, newest first
	Counts  map[string]int    `json:"counts"` // changes by status
	Changes []evolutionChange `json:"changes"`
}

func (s *Server) taskEvolution(t task.Task) (evolutionView, error) {
	v := evolutionView{Policy: t.Evolve, Counts: map[string]int{}, Changes: []evolutionChange{}, Rounds: []store.RunRecord{}}
	if err := s.evolutionRounds(&v, t.ID); err != nil {
		return v, err
	}
	all, err := s.ledger().list()
	if err != nil {
		return v, err
	}
	now := s.Now()
	var items []inbox.Item // read once, only if some change has a metric
	for _, e := range all {
		if e.Task != t.ID {
			continue
		}
		v.Counts[e.Status]++
		c := evolutionChange{LedgerEntry: e}
		if e.Metric != "" {
			if items == nil {
				if items, err = s.Inbox.List(true); err != nil {
					return v, err
				}
			}
			since := measureItems(items, e.Metric, e.At, now)
			c.Since = &since
		}
		v.Changes = append(v.Changes, c)
	}
	sort.Slice(v.Changes, func(i, j int) bool { return v.Changes[i].At.After(v.Changes[j].At) })
	if len(v.Changes) > evolutionShown {
		v.Changes = v.Changes[:evolutionShown]
	}
	return v, nil
}

// evolutionRounds adds the feedback rounds the self-evolution task ran about this task (its evolve-<task> lane).
func (s *Server) evolutionRounds(v *evolutionView, id string) error {
	if s.Evolve == nil || s.Evolve.Task == "" || s.Evolve.Task == id {
		return nil
	}
	runs, err := s.Store.Runs(s.Evolve.Task, 0)
	if err != nil {
		return err
	}
	for _, r := range runs { // newest first
		if r.Job == "evolve-"+id {
			v.Rounds = append(v.Rounds, r)
			if len(v.Rounds) == roundsShown {
				break
			}
		}
	}
	return nil
}
