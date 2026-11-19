package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigil/internal/inbox"
)

// Learning from the owner's decisions: per kind of to-do, Vigil tracks how often the draft is taken as is
// and how often raising it was noise (trust.go). Past a threshold it suggests a standing rule; a rule takes
// effect only when the owner accepts it, and lands in the task's long-term memory, which every run reads.
const (
	learnWindow     = trustWindow
	ruleMute        = 30 * 24 * time.Hour // a declined suggestion is not repeated for this long
	ruleRefPrefix   = "rule:"
	proposeEvidence = 5 // recent to-dos listed as evidence on a suggestion

	authorizeMin      = 5 // drafted to-dos before suggesting "reply directly"
	authorizeAdoption = 0.8
	quietMin          = 8 // reasoned samples before suggesting "stop reporting this"
	quietNoise        = 0.8
	revokeMin         = 3 // drafted to-dos since the authorization before suggesting taking it back
	revokeAdoption    = 0.5
)

// ruleSuggestion describes what a kind's record would become.
type ruleSuggestion struct {
	title string
	rule  string
}

func authorizeSuggestion(k KindTrust) ruleSuggestion {
	return ruleSuggestion{
		title: fmt.Sprintf("「%s」类待办 %d 条里 %.0f%% 照建议原样发出", k.Kind, k.Drafted, k.Adoption*100),
		rule: fmt.Sprintf("我已授权：「%s」类事情，有证据的明确答案直接以我的口吻回复，不用再进待办；"+
			"需要许诺、写操作、或拿不准的照旧写待办。", k.Kind),
	}
}

func quietSuggestion(k KindTrust) ruleSuggestion {
	return ruleSuggestion{
		title: fmt.Sprintf("「%s」类待办 %d 条里 %.0f%% 你标了不该找我", k.Kind, k.Samples, k.Noise*100),
		rule:  fmt.Sprintf("我已授权：「%s」类事情不用再报给我，也不用回复，在本轮结果里记一句即可。", k.Kind),
	}
}

// decisionFor classifies how an answer settles an item.
func decisionFor(it inbox.Item, mode, text string) string {
	switch mode {
	case TodoDismiss:
		return inbox.DecisionDismissed
	case TodoSend:
		if strings.TrimSpace(text) == strings.TrimSpace(it.Reply) {
			return inbox.DecisionAsIs
		}
		return inbox.DecisionEdited
	}
	return inbox.DecisionInstructed
}

// learn records the decision and, when it completes a streak, suggests a rule.
func (s *Server) learn(it inbox.Item, decision string) {
	s.learnFrom(it, decision, true)
}

// learnFrom is learn with a say on whether the decision is feedback worth waking the evolver for.
func (s *Server) learnFrom(it inbox.Item, decision string, feedback bool) {
	if it.Kind == "" || it.TaskID == inbox.SystemTask || isDecisionCard(it) {
		return
	}
	if _, err := s.Inbox.SetDecision(it.ID, decision); err != nil {
		s.logf("todo %s: record decision: %v", it.ID, err)
		return
	}
	if fb := feedbackFor(it, decision); feedback && fb != "" {
		s.wakeEvolve(fb)
	}
	s.proposeFor(it.TaskID, it.Kind)
}

// proposeFor looks at a kind's record after a decision and suggests giving the agent more say, taking
// it back, or no longer reporting the kind, when the numbers are there.
func (s *Server) proposeFor(taskID, kind string) {
	items, err := s.Inbox.List(true)
	if err != nil {
		s.logf("learn %s/%s: %v", taskID, kind, err)
		return
	}
	memory, _ := s.Store.ReadDoc(taskID, "memory.md")
	k, ok := kindTrust(trustFor(items, memory, s.handReplies(), taskID, s.Now()), kind)
	if !ok {
		return
	}
	if k.Authorized {
		s.proposeRevoke(items, memory, taskID, kind)
	} else if k.Drafted >= authorizeMin && k.Adoption >= authorizeAdoption {
		s.suggestRule(items, taskID, kind, inbox.DecisionAsIs, authorizeSuggestion(k))
	}
	if k.Samples >= quietMin && k.Noise >= quietNoise {
		s.suggestRule(items, taskID, kind, inbox.DecisionNoise, quietSuggestion(k))
	}
}

func kindTrust(kinds []KindTrust, kind string) (KindTrust, bool) {
	for _, k := range kinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return KindTrust{}, false
}

// suggestRule raises a rule suggestion for a kind, listing its recent to-dos as evidence.
func (s *Server) suggestRule(items []inbox.Item, taskID, kind, decision string, sug ruleSuggestion) {
	ref := ruleRefPrefix + taskID + ":" + kind + ":" + decision
	if !s.maySuggest(ref, taskID, kind) {
		return
	}
	var ev strings.Builder
	for _, r := range recentOfKind(items, taskID, kind, s.Now()) {
		fmt.Fprintf(&ev, "- %s %s（%s）", r.Updated.Format("01-02 15:04"), r.Title, r.Decision)
		if r.Answer != "" && r.Decision == inbox.DecisionAsIs {
			fmt.Fprintf(&ev, "\n  发出：%s", clipRunes(oneLine(r.Answer), 120))
		}
		ev.WriteString("\n")
	}
	s.gateProposal(inbox.Item{TaskID: taskID, Kind: "规则建议", Ref: ref,
		Title:    "规则建议：" + sug.title,
		Detail:   "采纳后写进 " + taskID + " 的长期记忆，以后每轮都按它办；可以先改措辞再采纳。不采纳的话 30 天内不再提。",
		Analysis: "最近的几次：\n" + ev.String(),
		Reply:    sug.rule}, kind)
}

// proposeRevoke suggests taking an authorization back when the drafts since then are mostly not taken.
func (s *Server) proposeRevoke(items []inbox.Item, memory, taskID, kind string) {
	line, at := authorizedIn(memory, kind)
	var since []inbox.Item
	for _, it := range items {
		if !it.Updated.Before(at) {
			since = append(since, it)
		}
	}
	k, ok := kindTrust(trustFor(since, memory, s.handReplies(), taskID, s.Now()), kind)
	if !ok || k.Drafted < revokeMin || k.Adoption >= revokeAdoption {
		return
	}
	ref := revokeRefPrefix + taskID + ":" + kind
	if last, found, err := s.Inbox.LatestByRef(ref); err != nil || found &&
		(last.Status != inbox.StatusDone || s.Now().Sub(last.Updated) < ruleMute) {
		return
	}
	added, created, err := s.Inbox.Add(inbox.Item{TaskID: taskID, Kind: "收回授权", Ref: ref,
		Title:    fmt.Sprintf("收回：「%s」类放权后 %d 条只有 %.0f%% 照原样发出", kind, k.Drafted, k.Adoption*100),
		Analysis: "放权后这类待办的建议多数被你改写或改成指示，说明它还没摸准你的意思。",
		Reply:    memoryNote(line)})
	if err != nil {
		s.logf("learn %s/%s: suggest revoke: %v", taskID, kind, err)
		return
	}
	if created {
		s.todoChanged(added, "todo.added")
	}
}

var memoryLinePrefix = regexp.MustCompile(`^-?\s*(\d{4}-\d{2}-\d{2}\s*)?`)

// memoryNote strips a memory line's "- <date> " prefix, leaving the note as RemoveMemory matches it.
func memoryNote(line string) string {
	return strings.TrimSpace(memoryLinePrefix.ReplaceAllString(strings.TrimSpace(line), ""))
}

// gateProposal raises a rule suggestion for a kind, after replaying it on the kind's past rounds: when the
// rule would move more decisions away from what the owner did than toward it, it is not raised but noted
// in the change ledger. Without a replayer (not configured) it is raised directly.
func (s *Server) gateProposal(it inbox.Item, kind string) {
	if s.Replayer.Run == nil && s.Replayer.Bin == "" {
		s.raiseProposal(it)
		return
	}
	if _, busy := s.replaying.LoadOrStore(it.Ref, true); busy {
		return // its replay is already running
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.replaying.Delete(it.Ref)
		rep, err := s.Replay(s.lifetime(), ReplayRequest{Task: it.TaskID, Doc: "memory.md", After: it.Reply, Kind: kind})
		if rep.Status == "failed" {
			s.logf("suggest rule %s: replay interrupted, will try again on a later decision: %v", it.Ref, err)
			return
		}
		if err != nil {
			s.logf("suggest rule %s: replay: %v", it.Ref, err)
			s.raiseProposal(it)
			return
		}
		if rep.Farther > rep.Closer {
			s.logf("suggest rule %s held back by replay %s: %s", it.Ref, rep.ID, rep.Summary)
			now := s.Now()
			if err := s.ledger().add(LedgerEntry{ID: "ev-" + randHex(3), At: now, Task: it.TaskID, Target: it.TaskID + " memory.md",
				Change: clipRunes("（未提出）"+it.Reply, ledgerFieldLen), Why: clipRunes(it.Title, ledgerFieldLen),
				Expect: "回放否决：" + rep.Summary + "（" + rep.ID + "）", ReviewAt: now, Status: LedgerRejected, Closed: now, Ref: it.Ref}); err != nil {
				s.logf("ledger: %v", err)
			}
			return
		}
		it.Analysis = "回放：" + rep.Summary + "\n\n" + it.Analysis
		it.Link = "/api/evolve/replay/" + rep.ID
		s.raiseProposal(it)
	}()
}

func (s *Server) raiseProposal(it inbox.Item) {
	added, created, err := s.Inbox.Add(it)
	if err != nil {
		s.logf("suggest rule %s: %v", it.Ref, err)
		return
	}
	if created {
		s.todoChanged(added, "todo.added")
	}
}

// recentOfKind returns a task's settled to-dos of one kind, newest first, at most proposeEvidence.
func recentOfKind(items []inbox.Item, taskID, kind string, now time.Time) []inbox.Item {
	cutoff := now.Add(-learnWindow)
	var out []inbox.Item
	for _, it := range items {
		if it.TaskID == taskID && it.Kind == kind && it.Decision != "" && it.Updated.After(cutoff) {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	if len(out) > proposeEvidence {
		out = out[:proposeEvidence]
	}
	return out
}

// maySuggest reports whether a rule may be suggested (again): not while one is open, not again after the
// owner accepted it, not for ruleMute after he declined it or took the authorization back.
func (s *Server) maySuggest(ref, taskID, kind string) bool {
	if s.rejectedRecently(ref) {
		return false
	}
	last, found, err := s.Inbox.LatestByRef(ref)
	if err != nil {
		return false
	}
	if !found {
		return true
	}
	if last.Status != inbox.StatusDone {
		return false
	}
	if last.Decision == inbox.DecisionAsIs {
		revoked, ok := s.revokedSince(taskID, kind)
		return ok && revoked.After(last.Updated) && s.Now().Sub(revoked) >= ruleMute
	}
	return s.Now().Sub(last.Updated) >= ruleMute
}

// rejectedRecently reports whether replay held this suggestion back within ruleMute.
func (s *Server) rejectedRecently(ref string) bool {
	entries, err := s.ledger().list()
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Ref == ref && e.Status == LedgerRejected && s.Now().Sub(e.At) < ruleMute {
			return true
		}
	}
	return false
}

// isRule reports whether an item is a rule suggestion.
func isRule(it inbox.Item) bool { return strings.HasPrefix(it.Ref, ruleRefPrefix) }

// settleRule handles the answer to a rule suggestion: accepting writes the (possibly edited) rule into the
// task's memory; dismissing declines it. Only an open suggestion can be settled, and claiming it (Answer)
// happens atomically before memory is written, so two answers can never write the rule twice.
func (s *Server) settleRule(it inbox.Item, mode, text string) (inbox.Item, error) {
	if it.Status != inbox.StatusOpen {
		return inbox.Item{}, todoErr(inbox.ErrClosed)
	}
	if mode == TodoDismiss {
		closed, err := s.Inbox.CloseIfOpen(it.ID, "没采纳")
		if err != nil {
			return inbox.Item{}, todoErr(err)
		}
		if closed, err = s.Inbox.SetDecision(it.ID, inbox.DecisionDismissed); err != nil {
			return inbox.Item{}, todoErr(err)
		}
		s.todoChanged(closed, "todo.closed")
		return closed, nil
	}
	rule := strings.TrimSpace(text)
	if rule == "" {
		return inbox.Item{}, badRequest("规则内容不能为空")
	}
	if _, err := s.Inbox.Answer(it.ID, rule); err != nil {
		return inbox.Item{}, todoErr(err) // someone else settled it first
	}
	if err := s.Store.AppendMemory(it.TaskID, []string{rule}, s.Now()); err != nil {
		if _, nerr := s.Inbox.Note(it.ID, "写入长期记忆失败："+err.Error(), true); nerr != nil {
			s.logf("todo %s: reopen after failed memory write: %v", it.ID, nerr)
		}
		return inbox.Item{}, fmt.Errorf("写入长期记忆：%w", err)
	}
	if _, err := s.Inbox.SetDecision(it.ID, inbox.DecisionAsIs); err != nil {
		return inbox.Item{}, todoErr(err)
	}
	closed, err := s.Inbox.Close(it.ID, "已采纳并写进 "+it.TaskID+" 的长期记忆："+rule)
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	s.changed(it.TaskID, "memory")
	s.todoChanged(closed, "todo.closed")
	return closed, nil
}
