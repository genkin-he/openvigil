package server

import (
	"sort"
	"strings"
	"time"

	"vigil/internal/inbox"
)

// Autonomy is the self-evolution task's north star: how often the owner still has to step in. Each to-do
// is counted by how it was settled; "effort" is what cost the owner writing (a typed instruction, an edited
// reply, a reply sent by hand elsewhere). Clicking a choice or sending a draft as is costs a click.
const (
	autonomyDays  = 7
	autonomyKinds = 10
	// pickedPrefix starts the answer AnswerChoice records; a "。补充：" after it means the owner typed too.
	pickedPrefix = "我选了「"
	pickedRemark = "。补充："
)

// autonomyCount is how a set of to-dos was settled.
type autonomyCount struct {
	Total     int `json:"total"`
	AsIs      int `json:"as_is"`     // the draft went out unchanged
	Picked    int `json:"picked"`    // one of the item's buttons, nothing typed
	Typed     int `json:"typed"`     // an instruction the owner wrote
	Edited    int `json:"edited"`    // the draft, rewritten
	Elsewhere int `json:"elsewhere"` // answered by hand in Slack or the ticket
	Dismissed int `json:"dismissed"` // nothing to do (noise, stale, dismissed)
	Auto      int `json:"auto"`      // settled without the owner: the task, the other side or Vigil itself closed it
	Open      int `json:"open"`      // not settled yet
	Effort    int `json:"effort"`    // typed + edited + elsewhere
}

type autonomyDay struct {
	Date string `json:"date"` // MM-DD, local time
	Task string `json:"task"`
	autonomyCount
}

type autonomyKind struct {
	Task string `json:"task"`
	Kind string `json:"kind"`
	autonomyCount
}

// autonomyView is the last week of to-dos: per day and task (newest first), and the kinds that cost the
// owner the most.
type autonomyView struct {
	Days  []autonomyDay  `json:"days"`
	Kinds []autonomyKind `json:"kinds"`
}

func (c *autonomyCount) add(it inbox.Item) {
	c.Total++
	switch it.Decision {
	case "":
		switch {
		case it.Status != inbox.StatusDone:
			c.Open++
		case strings.HasPrefix(it.Ref, dialogTodoRefPrefix) && strings.HasPrefix(it.Outcome, dialogPressed):
			c.Picked++ // the owner pressed one of the dialog's buttons on the card
		default:
			c.Auto++
		}
	case inbox.DecisionAsIs:
		c.AsIs++
	case inbox.DecisionInstructed:
		if strings.HasPrefix(it.Answer, pickedPrefix) && !strings.Contains(it.Answer, pickedRemark) {
			c.Picked++
		} else {
			c.Typed++
		}
	case inbox.DecisionEdited:
		c.Edited++
	case inbox.DecisionHandledElsewhere:
		c.Elsewhere++
	default:
		c.Dismissed++
	}
	c.Effort = c.Typed + c.Edited + c.Elsewhere
}

func autonomyOf(items []inbox.Item, now time.Time) autonomyView {
	since := now.AddDate(0, 0, -(autonomyDays - 1))
	since = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, now.Location())
	days := map[[2]string]*autonomyDay{}
	kinds := map[[2]string]*autonomyKind{}
	for _, it := range items {
		if it.Created.Before(since) || strings.HasPrefix(it.Ref, selfTestRef+":") {
			continue // Vigil's own end-to-end test cards are not the owner's work
		}
		date := it.Created.In(now.Location()).Format("01-02")
		d := days[[2]string{date, it.TaskID}]
		if d == nil {
			d = &autonomyDay{Date: date, Task: it.TaskID}
			days[[2]string{date, it.TaskID}] = d
		}
		d.add(it)
		k := kinds[[2]string{it.TaskID, it.Kind}]
		if k == nil {
			k = &autonomyKind{Task: it.TaskID, Kind: it.Kind}
			kinds[[2]string{it.TaskID, it.Kind}] = k
		}
		k.add(it)
	}
	v := autonomyView{Days: []autonomyDay{}, Kinds: []autonomyKind{}}
	for _, d := range days {
		v.Days = append(v.Days, *d)
	}
	sort.Slice(v.Days, func(i, j int) bool {
		if v.Days[i].Date != v.Days[j].Date {
			return v.Days[i].Date > v.Days[j].Date
		}
		return v.Days[i].Task < v.Days[j].Task
	})
	for _, k := range kinds {
		v.Kinds = append(v.Kinds, *k)
	}
	sort.Slice(v.Kinds, func(i, j int) bool {
		a, b := v.Kinds[i], v.Kinds[j]
		if a.Effort != b.Effort {
			return a.Effort > b.Effort
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Task+a.Kind < b.Task+b.Kind
	})
	if len(v.Kinds) > autonomyKinds {
		v.Kinds = v.Kinds[:autonomyKinds]
	}
	return v
}

// taskAutonomy is one task's week of to-dos, for its page.
func (s *Server) taskAutonomy(id string) (autonomyView, error) {
	items, err := s.Inbox.List(true)
	if err != nil {
		return autonomyView{}, err
	}
	mine := items[:0:0]
	for _, it := range items {
		if it.TaskID == id {
			mine = append(mine, it)
		}
	}
	return autonomyOf(mine, s.Now()), nil
}
