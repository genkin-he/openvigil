package server

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigil/internal/inbox"
)

// Trust per kind of to-do: how often the agent's draft is taken as is, and how often raising it was
// noise. Only reasoned decisions count: a to-do that went stale, or an old dismissal without a reason,
// says nothing either way.
const trustWindow = 30 * 24 * time.Hour

// KindTrust is one kind's record over trustWindow.
type KindTrust struct {
	Kind         string    `json:"kind"`
	Samples      int       `json:"samples"` // denominator of Noise
	Drafted      int       `json:"drafted"` // denominator of Adoption: decided items that came with a draft
	Adoption     float64   `json:"adoption"`
	Noise        float64   `json:"noise"`
	Authorized   bool      `json:"authorized"`
	AuthorizedAt time.Time `json:"authorized_at,omitzero"`
	Proposal     string    `json:"proposal,omitempty"` // latest rule suggestion for the kind: open / accepted / declined
}

var authorizedLine = regexp.MustCompile(`^-?\s*(\d{4}-\d{2}-\d{2})?\s*我已授权`)

// trustFor computes the per-kind record of one task's to-dos; memory is the task's long-term memory,
// where accepted authorizations live as「我已授权：「<kind>」…」lines.
func trustFor(items []inbox.Item, memory, taskID string, now time.Time) []KindTrust {
	cutoff := now.Add(-trustWindow)
	byKind := map[string]*KindTrust{}
	counts := map[string]map[string]int{} // kind → decision → drafted count
	for _, it := range items {
		if it.TaskID != taskID || it.Kind == "" || it.Decision == "" || it.Updated.Before(cutoff) || isDecisionCard(it) {
			continue
		}
		k := byKind[it.Kind]
		if k == nil {
			k = &KindTrust{Kind: it.Kind}
			byKind[it.Kind], counts[it.Kind] = k, map[string]int{}
		}
		switch it.Decision {
		case inbox.DecisionAsIs, inbox.DecisionEdited, inbox.DecisionInstructed:
			k.Samples++
			if it.Reply != "" {
				k.Drafted++
				counts[it.Kind][it.Decision]++
			}
		case inbox.DecisionNoise:
			k.Samples++
			counts[it.Kind][inbox.DecisionNoise]++
		case inbox.DecisionHandledElsewhere:
			k.Samples++
		}
	}
	out := make([]KindTrust, 0, len(byKind))
	for kind, k := range byKind {
		if k.Samples == 0 {
			continue // only stale or reasonless dismissals: nothing to say about this kind
		}
		if k.Drafted > 0 {
			k.Adoption = float64(counts[kind][inbox.DecisionAsIs]) / float64(k.Drafted)
		}
		if k.Samples > 0 {
			k.Noise = float64(counts[kind][inbox.DecisionNoise]) / float64(k.Samples)
		}
		line, at := authorizedIn(memory, kind)
		k.Authorized, k.AuthorizedAt = line != "", at
		out = append(out, *k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// authorizedIn returns memory's authorization line naming the kind ("" when none) and its date if it has one.
func authorizedIn(memory, kind string) (string, time.Time) {
	for _, line := range strings.Split(memory, "\n") {
		line = strings.TrimSpace(line)
		m := authorizedLine.FindStringSubmatch(line)
		if m == nil || !strings.Contains(line, "「"+kind+"」") {
			continue
		}
		at, _ := time.ParseInLocation("2006-01-02", m[1], time.Local)
		return line, at
	}
	return "", time.Time{}
}

func (s *Server) taskTrust(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	if _, err := s.Store.Get(id); err != nil {
		return nil, badRequest("%v", err)
	}
	items, err := s.Inbox.List(true)
	if err != nil {
		return nil, err
	}
	memory, _ := s.Store.ReadDoc(id, "memory.md")
	kinds := trustFor(items, memory, id, s.Now())
	for i := range kinds {
		kinds[i].Proposal = proposalState(items, id, kinds[i].Kind)
	}
	return kinds, nil
}

// proposalState is the state of the latest rule suggestion for a kind, "" when there was none.
func proposalState(items []inbox.Item, taskID, kind string) string {
	var latest inbox.Item
	found := false
	prefix := ruleRefPrefix + taskID + ":" + kind + ":"
	for _, it := range items {
		if strings.HasPrefix(it.Ref, prefix) && (!found || it.Updated.After(latest.Updated)) {
			latest, found = it, true
		}
	}
	switch {
	case !found:
		return ""
	case latest.Status != inbox.StatusDone:
		return "open"
	case latest.Decision == inbox.DecisionDismissed:
		return "declined"
	}
	return "accepted"
}
