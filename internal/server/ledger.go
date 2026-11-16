package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// The change ledger: every change the self-evolution task makes (or gets approved) is written down with
// what it should improve and when to look again. When that day comes the task checks the evidence and
// keeps the change or reverts it, so changes that did not help do not pile up as rules nobody questions.
const (
	ledgerFile       = "evolve.jsonl"
	LedgerWatching   = "watching"
	LedgerKept       = "kept"
	LedgerReverted   = "reverted"
	LedgerRejected   = "rejected_by_replay" // a suggestion replay showed would move decisions away from the owner's
	ledgerDefaultDay = 7
	ledgerMaxDays    = 60
	ledgerFieldLen   = 2000
)

// LedgerEntry is one change and its verdict.
type LedgerEntry struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Task     string    `json:"task"`             // the task whose behaviour the change is about
	Target   string    `json:"target"`           // file (and section) changed
	Change   string    `json:"change"`           // what changed, before → after
	Why      string    `json:"why"`              // the signals behind it
	Expect   string    `json:"expect"`           // what should be different, checkable from runs / to-dos / corrections
	Backup   string    `json:"backup,omitempty"` // where the previous version is, for a revert
	Todo     string    `json:"todo,omitempty"`   // the to-do the owner approved it in, when it needed approval
	ReviewAt time.Time `json:"review_at"`
	Status   string    `json:"status"`
	Verdict  string    `json:"verdict,omitempty"`
	Closed   time.Time `json:"closed_at,omitzero"`
	Ref      string    `json:"ref,omitempty"` // for a suggestion held back by replay: its to-do ref, so it is not replayed again soon
}

type ledgerStore = recordLog[LedgerEntry]

// closeChange records the verdict on a change still being watched.
func closeChange(l *ledgerStore, id, status, verdict string, at time.Time) (LedgerEntry, error) {
	return l.update(id, func(e LedgerEntry) (LedgerEntry, error) {
		if e.Status != LedgerWatching {
			return e, badRequest("变更 %s 已经是 %s，不能再判", id, e.Status)
		}
		e.Status, e.Verdict, e.Closed = status, verdict, at
		return e, nil
	})
}

func (s *Server) ledger() *ledgerStore {
	s.ledgerOnce.Do(func() {
		s.ledgerLog = &ledgerStore{path: filepath.Join(s.Store.Root(), ledgerFile), id: func(e LedgerEntry) string { return e.ID }}
	})
	return s.ledgerLog
}

type ledgerBody struct {
	Task       string `json:"task"`
	Target     string `json:"target"`
	Change     string `json:"change"`
	Why        string `json:"why"`
	Expect     string `json:"expect"`
	Backup     string `json:"backup"`
	Todo       string `json:"todo"`
	ReviewDays int    `json:"review_days"`
}

func (s *Server) ledgerAdd(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b ledgerBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	for name, v := range map[string]string{"task": b.Task, "target": b.Target, "change": b.Change, "why": b.Why, "expect": b.Expect} {
		if strings.TrimSpace(v) == "" {
			return nil, badRequest("%s 不能为空：没写清改了什么、为什么、期望什么，以后就没法判断它有没有用", name)
		}
	}
	if _, err := s.Store.Get(b.Task); err != nil {
		return nil, badRequest("没有任务 %s", b.Task)
	}
	days := b.ReviewDays
	if days == 0 {
		days = ledgerDefaultDay
	}
	if days < 1 || days > ledgerMaxDays {
		return nil, badRequest("review_days 要在 1～%d 之间", ledgerMaxDays)
	}
	now := s.Now()
	e := LedgerEntry{ID: "ev-" + randHex(3), At: now, Task: b.Task, Target: clipRunes(b.Target, 200),
		Change: clipRunes(b.Change, ledgerFieldLen), Why: clipRunes(b.Why, ledgerFieldLen), Expect: clipRunes(b.Expect, ledgerFieldLen),
		Backup: clipRunes(b.Backup, 300), Todo: clipRunes(b.Todo, 40), ReviewAt: now.Add(time.Duration(days) * 24 * time.Hour), Status: LedgerWatching}
	if err := s.ledger().add(e); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Server) ledgerList(_ http.ResponseWriter, _ *http.Request) (any, error) {
	all, err := s.ledger().list()
	if err != nil {
		return nil, err
	}
	if all == nil {
		all = []LedgerEntry{}
	}
	return all, nil
}

type verdictBody struct {
	Status  string `json:"status"`
	Verdict string `json:"verdict"`
}

func (s *Server) ledgerVerdict(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b verdictBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Status != LedgerKept && b.Status != LedgerReverted {
		return nil, badRequest("status 只能是 kept 或 reverted")
	}
	if strings.TrimSpace(b.Verdict) == "" {
		return nil, badRequest("verdict 要写依据：看了哪些数据、和改之前比怎样")
	}
	return closeChange(s.ledger(), r.PathValue("id"), b.Status, clipRunes(b.Verdict, ledgerFieldLen), s.Now())
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:2*n]
	}
	return hex.EncodeToString(b)
}
