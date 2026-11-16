package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/store"
)

// Corrections: when an agent's reply sent with the owner's account was wrong, the owner says so in that
// conversation, @-ing himself with「ai 回错了…」. The watch still handles the message (works out the
// lesson, proposes taking back the authorization behind it); Vigil records every correction so the
// weekly review can see how often replies go wrong, and settles the take-back cards.
const (
	correctionsFile = "corrections.jsonl"
	revokeRefPrefix = "revoke:"
	maxCorrections  = 50
)

// correctionText matches the owner's way of saying an agent reply was wrong.
// It needs the「ai」(or 机器人 / 值守) in front, so the owner's own「我回错了」is not taken for one.
var correctionText = regexp.MustCompile(`(?i)(ai|机器人|值守)\s*(回|答|说|发)错`)

// Correction is one「回错了」the owner wrote.
type Correction struct {
	At      time.Time `json:"at"`
	Channel string    `json:"channel"`
	TS      string    `json:"ts"`
	Thread  string    `json:"thread,omitempty"`
	Text    string    `json:"text"`
}

type corrections = recordLog[Correction]

func correctionKey(x Correction) string {
	if x.Channel == "" || x.TS == "" {
		return ""
	}
	return x.Channel + ":" + x.TS
}

// correctionsSince returns the corrections at or after from, newest first.
func correctionsSince(c *corrections, from time.Time) ([]Correction, error) {
	all, err := c.list()
	if err != nil {
		return nil, err
	}
	var out []Correction
	for _, x := range all {
		if !x.At.Before(from) {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

func (s *Server) correctionLog() *corrections {
	s.correctOnce.Do(func() {
		s.correct = &corrections{path: filepath.Join(s.Store.Root(), correctionsFile), id: correctionKey}
	})
	return s.correct
}

// noteCorrection records an event line when it is the owner saying a reply was wrong. The line is not
// taken: the watch handles it as well.
func (s *Server) noteCorrection(g []string) {
	channel, ts, thread, self, text := g[1], g[2], g[3], g[4], stripTags(g[5])
	if self != "1" || !correctionText.MatchString(text) {
		return
	}
	if thread == "-" {
		thread = ""
	}
	x := Correction{At: s.Now(), Channel: channel, TS: ts, Thread: thread, Text: clipRunes(text, signalTextLen)}
	_, seen, err := s.correctionLog().addUnless(x, func(Correction) bool { return false })
	if err != nil {
		s.logf("correction %s:%s: %v", channel, ts, err)
		return
	}
	if seen {
		return // the same message delivered again (reconnect, backfill)
	}
	s.wakeEvolve("纠错 " + channel + ":" + ts + "：" + text)
}

// isRevoke reports whether an item proposes taking a line out of a task's memory.
func isRevoke(it inbox.Item) bool { return strings.HasPrefix(it.Ref, revokeRefPrefix) }

// isDecisionCard reports whether an item is settled by a plain yes or no (rule, take-back and patch cards).
func isDecisionCard(it inbox.Item) bool { return isRule(it) || isRevoke(it) || isPatch(it) }

// settleRevoke handles the answer to a take-back card: yes removes the line (its Reply) from the task's
// memory, no keeps it. Like settleRule, the item is claimed before memory changes.
func (s *Server) settleRevoke(it inbox.Item, mode string) (inbox.Item, error) {
	if it.Status != inbox.StatusOpen {
		return inbox.Item{}, todoErr(inbox.ErrClosed)
	}
	if mode == TodoDismiss {
		closed, err := s.Inbox.CloseIfOpen(it.ID, "保留，没收回")
		if err != nil {
			return inbox.Item{}, todoErr(err)
		}
		s.todoChanged(closed, "todo.closed")
		return closed, nil
	}
	line := strings.TrimSpace(it.Reply)
	if line == "" {
		return inbox.Item{}, badRequest("这条没写要收回哪一行记忆")
	}
	if _, err := s.Inbox.Answer(it.ID, line); err != nil {
		return inbox.Item{}, todoErr(err)
	}
	n, err := s.Store.RemoveMemory(it.TaskID, line)
	if err != nil {
		why := "从长期记忆删除失败：" + err.Error()
		if errors.Is(err, store.ErrNoMemoryLine) {
			why = "长期记忆里找不到这一行（可能已经改过或删过），没动记忆"
		}
		if _, nerr := s.Inbox.Note(it.ID, why, true); nerr != nil {
			s.logf("todo %s: reopen after failed revoke: %v", it.ID, nerr)
		}
		return inbox.Item{}, badRequest("%s", why)
	}
	if _, err := s.Inbox.SetDecision(it.ID, inbox.DecisionAsIs); err != nil {
		return inbox.Item{}, todoErr(err)
	}
	closed, err := s.Inbox.Close(it.ID, fmt.Sprintf("已从 %s 的长期记忆收回（%d 行）：%s", it.TaskID, n, line))
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	s.changed(it.TaskID, "memory")
	s.todoChanged(closed, "todo.closed")
	return closed, nil
}

// revokedSince reports whether the authorization for a task's kind was taken back, and when.
func (s *Server) revokedSince(taskID, kind string) (time.Time, bool) {
	last, found, err := s.Inbox.LatestByRef(revokeRefPrefix + taskID + ":" + kind)
	if err != nil || !found || last.Status != inbox.StatusDone || last.Decision != inbox.DecisionAsIs {
		return time.Time{}, false
	}
	return last.Updated, true
}

// clipCorrections keeps the retro listing short.
func clipCorrections(xs []Correction) []Correction {
	if len(xs) > maxCorrections {
		return xs[:maxCorrections]
	}
	return xs
}
