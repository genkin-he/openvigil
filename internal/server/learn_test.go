package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
)

func TestRepeatedAsIsSendsSuggestRule(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	settle := func(i int, edit bool) {
		it, _, err := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "问进度", Title: fmt.Sprintf("同事%d：进度？", i),
			Reply: "今天发 canary", Ref: fmt.Sprintf("C1:%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		text := it.Reply
		if edit {
			text += "，明早上线"
		}
		if _, err := s.AnswerTodo(it.ID, TodoSend, text); err != nil {
			t.Fatal(err)
		}
	}
	rules := func() []inbox.Item {
		var out []inbox.Item
		items, _ := s.Inbox.List(false)
		for _, it := range items {
			if isRule(it) {
				out = append(out, it)
			}
		}
		return out
	}

	settle(1, false)
	settle(2, true) // an edit counts against the adoption rate
	settle(3, false)
	settle(4, false)
	if len(rules()) != 0 {
		t.Fatal("rule suggested before 5 drafted to-dos at 80% adoption")
	}
	settle(5, false)
	got := rules()
	if len(got) != 1 || !strings.Contains(got[0].Reply, "「问进度」") || !strings.Contains(got[0].Analysis, "同事5") {
		t.Fatalf("suggestion = %+v", got)
	}
	settle(6, false)
	if len(rules()) != 1 {
		t.Fatal("suggested twice")
	}

	// Accepting an edited rule writes it into the task's memory.
	if _, err := s.AnswerTodo(got[0].ID, TodoSend, "问进度的直接回"); err != nil {
		t.Fatal(err)
	}
	mem, _ := s.Store.ReadDoc("slack", "memory.md")
	if !strings.Contains(mem, "问进度的直接回") {
		t.Fatalf("memory = %q", mem)
	}
	s.Engine.Wait()
}

func TestDeclinedRuleIsNotRepeated(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	for i := 0; i < 11; i++ {
		it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "通知", Title: "通知", Ref: fmt.Sprintf("n:%d", i)})
		if _, err := s.AnswerTodoReason(it.ID, TodoDismiss, "", inbox.DecisionNoise); err != nil {
			t.Fatal(err)
		}
		if i == quietMin-1 {
			rule, ok, _ := s.Inbox.OpenByRef("rule:slack:通知:noise")
			if !ok {
				t.Fatal("no suggestion after 8 noise dismissals")
			}
			if _, err := s.AnswerTodo(rule.ID, TodoDismiss, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, ok, _ := s.Inbox.OpenByRef("rule:slack:通知:noise"); ok {
		t.Fatal("declined suggestion came back")
	}
}

func TestRuleCannotBeSettledTwice(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	rule, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "规则建议", Ref: "rule:slack:x:as_is", Title: "规则建议", Reply: "直接回"})
	if _, err := s.AnswerTodo(rule.ID, TodoDismiss, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerTodo(rule.ID, TodoSend, "直接回"); err == nil {
		t.Fatal("a declined rule was accepted afterwards")
	}
	if mem, _ := s.Store.ReadDoc("slack", "memory.md"); strings.Contains(mem, "直接回") {
		t.Fatalf("declined rule reached memory: %q", mem)
	}
}

func TestSlackReplyToRuleCardNeedsClearAnswer(t *testing.T) {
	s, _ := mirrorServer(t)
	rule, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "规则建议", Ref: "rule:slack:y:as_is", Title: "规则建议", Reply: "直接回"})
	for _, text := range []string{"为什么？", "先等等"} {
		s.applySlackAnswer(rule, text)
	}
	if got, _ := s.Inbox.Get(rule.ID); got.Status != inbox.StatusOpen {
		t.Fatalf("an unclear reply settled the rule: %+v", got)
	}
	s.applySlackAnswer(rule, "不要")
	if got, _ := s.Inbox.Get(rule.ID); got.Status != inbox.StatusDone || got.Decision != inbox.DecisionDismissed {
		t.Fatalf("「不要」should decline: %+v", got)
	}
	if mem, _ := s.Store.ReadDoc("slack", "memory.md"); strings.Contains(mem, "直接回") || strings.Contains(mem, "不要") {
		t.Fatalf("memory = %q", mem)
	}
}

// settleN raises n to-dos of a kind and settles each with the given decision.
var settleSeq int

func settleN(t *testing.T, s *Server, kind string, n int, decision string) {
	t.Helper()
	for range n {
		settleSeq++
		i := settleSeq
		it, _, err := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: kind, Title: fmt.Sprintf("%s %s %d", kind, decision, i),
			Reply: "草稿", Ref: fmt.Sprintf("%s:%s:%d", kind, decision, i)})
		if err != nil {
			t.Fatal(err)
		}
		switch decision {
		case inbox.DecisionAsIs:
			_, err = s.AnswerTodo(it.ID, TodoSend, "草稿")
		case inbox.DecisionEdited:
			_, err = s.AnswerTodo(it.ID, TodoSend, "改过的")
		default:
			_, err = s.AnswerTodoReason(it.ID, TodoDismiss, "", decision)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func openRule(s *Server, ref string) bool {
	_, ok, _ := s.Inbox.OpenByRef(ref)
	return ok
}

func TestNoProposalFromThreeDismissals(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	settleN(t, s, "通知", 3, inbox.DecisionNoise)
	if openRule(s, "rule:slack:通知:noise") {
		t.Fatal("3 noise dismissals must not be enough")
	}
}

func TestNoiseProposalNeedsEight(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	settleN(t, s, "通知", 7, inbox.DecisionNoise)
	if openRule(s, "rule:slack:通知:noise") {
		t.Fatal("7 samples must not be enough")
	}
	settleN(t, s, "通知", 1, inbox.DecisionAsIs)
	if !openRule(s, "rule:slack:通知:noise") {
		t.Fatal("8 samples at 7/8 noise should propose")
	}
}

func TestHandledElsewhereNeverCountsAsNoise(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	settleN(t, s, "通知", 10, inbox.DecisionHandledElsewhere)
	if openRule(s, "rule:slack:通知:noise") {
		t.Fatal("handled elsewhere is not noise")
	}
}

func TestAuthorizeNeedsFiveDrafted(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	settleN(t, s, "问进度", 4, inbox.DecisionAsIs)
	if openRule(s, "rule:slack:问进度:as_is") {
		t.Fatal("4 drafted must not be enough")
	}
	settleN(t, s, "问进度", 1, inbox.DecisionAsIs)
	if !openRule(s, "rule:slack:问进度:as_is") {
		t.Fatal("5 drafted at 100% should propose")
	}
}

func TestRevokeWhenAdoptionDropsAfterAuthorization(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	line := "我已授权：「问进度」类事情，有证据的明确答案直接以我的口吻回复"
	if err := s.Store.AppendMemory("slack", []string{line}, s.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	settleN(t, s, "问进度", 2, inbox.DecisionEdited)
	if openRule(s, "revoke:slack:问进度") {
		t.Fatal("2 samples after authorization must not be enough")
	}
	settleN(t, s, "问进度", 1, inbox.DecisionAsIs)
	got, ok, _ := s.Inbox.OpenByRef("revoke:slack:问进度")
	if !ok || !strings.Contains(got.Reply, "「问进度」") {
		t.Fatalf("1/3 adoption after authorization should propose taking it back: %+v %v", got, ok)
	}
}
