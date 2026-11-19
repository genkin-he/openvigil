package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"vigil/internal/slackctl"
	"vigil/internal/store"
)

// aftermathServer has one Slack task whose run at 1794369300 took in the given input lines.
func aftermathServer(t *testing.T, th *threadsByChannel, input string) (*Server, *fakeEvolve) {
	t.Helper()
	s, _ := mirrorServer(t)
	s.Mirror = nil
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")
	s.Threads = th
	start := time.Unix(1794369300, 0)
	if err := s.Store.AppendRun(store.RunRecord{ID: "r1", TaskID: "slack", Status: store.StatusOK, Input: input, StartedAt: start, EndedAt: start.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Unix(1794372000, 0) }
	return s, f
}

func aftermaths(t *testing.T, s *Server) []Aftermath {
	t.Helper()
	got, err := s.aftermathLog().list()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAPushbackAfterAnAgentReplyIsRecordedAndWakesSelfEvolution(t *testing.T) {
	th := &threadsByChannel{threads: map[string][]slackctl.Message{"C1:1794369200.0": {
		{TS: "1794369200.0", User: "U0LIN", Text: "sit 的推送为什么没到"},
		{TS: "1794369350.0", User: "U0SELF", Text: "是证书过期了，已经换了。 *发送工具* <@U0BOT>"},
		{TS: "1794369500.0", User: "U0ZHOU", Text: "+1"},
		{TS: "1794369600.0", User: "U0LIN", Text: "还是不行，刚又试了一次"},
	}}}
	s, f := aftermathServer(t, th, "10:00:00 SLACK C1:1794369200.0 thread=- user=U0LIN dm=0 mention=1 self=0 | sit 的推送为什么没到")

	s.watchAftermath(context.Background())
	s.watchAftermath(context.Background()) // a second pass adds nothing

	got := aftermaths(t, s)
	if len(got) != 1 {
		t.Fatalf("aftermaths = %+v", got)
	}
	a := got[0]
	if a.Task != "slack" || a.Run != "r1" || a.ReplyTS != "1794369350.0" || !strings.HasPrefix(a.Reply, "是证书过期了") ||
		strings.Join(a.Followups, "|") != "还是不行，刚又试了一次" || a.Verdict != VerdictPushback {
		t.Fatalf("aftermath = %+v", a)
	}
	if len(f.timers) != 1 {
		t.Fatal("a pushback did not wake self-evolution")
	}
}

func TestThanksAfterAnAgentReplyIsRecordedQuietly(t *testing.T) {
	th := &threadsByChannel{
		threads: map[string][]slackctl.Message{"D1:1794369200.0": {{TS: "1794369200.0", User: "U0LIN", Text: "权限开了吗"}}},
		history: map[string][]slackctl.Message{"D1": { // newest first
			{TS: "1794369700.0", User: "U0SELF", Text: "不客气"}, // the owner took over: later words are not about the reply
			{TS: "1794369600.0", User: "U0LIN", Text: "好的谢谢"},
			{TS: "1794369350.0", User: "U0SELF", Text: "开好了。 *发送工具*"},
			{TS: "1794369200.0", User: "U0LIN", Text: "权限开了吗"},
		}},
	}
	s, f := aftermathServer(t, th, "SLACK D1:1794369200.0 thread=- user=U0LIN dm=1 mention=1 self=0 | 权限开了吗")

	s.watchAftermath(context.Background())

	got := aftermaths(t, s)
	if len(got) != 1 || got[0].Verdict != VerdictThanks || len(got[0].Followups) != 1 {
		t.Fatalf("aftermaths = %+v", got)
	}
	if len(f.timers) != 0 {
		t.Fatal("thanks woke self-evolution")
	}
}

func TestOnlyAgentRepliesToOthersAreWatched(t *testing.T) {
	th := &threadsByChannel{threads: map[string][]slackctl.Message{
		"C1:1794369200.0": {
			{TS: "1794369200.0", User: "U0LIN", Text: "在吗"},
			{TS: "1794369350.0", User: "U0SELF", Text: "在"}, // the owner by hand
			{TS: "1794369400.0", User: "U0LIN", Text: "不对"},
		},
		"D0:1794369210.0": {
			{TS: "1794369210.0", User: "U0SELF", Text: "帮我看下 CI"},
			{TS: "1794369350.0", User: "U0SELF", Text: "CI 过了 *发送工具*"},
		},
	}}
	s, _ := aftermathServer(t, th, "SLACK C1:1794369200.0 thread=- user=U0LIN dm=0 mention=1 self=0 | 在吗\n"+
		"SLACK D0:1794369210.0 thread=- user=U0SELF dm=1 mention=0 self=1 | 帮我看下 CI")

	s.watchAftermath(context.Background())

	if got := aftermaths(t, s); len(got) != 0 {
		t.Fatalf("aftermaths = %+v", got)
	}
}

func TestVerdicts(t *testing.T) {
	for text, want := range map[string]string{
		"还是不行":         VerdictPushback,
		"不对吧，我看的是 sit": VerdictPushback,
		"好的，谢谢":        VerdictThanks,
		"👍":            VerdictThanks,
		"那 staging 呢":  VerdictFollowup,
		"是不是要重启一下":     VerdictFollowup,
		":pray: :+1:":  VerdictThanks,
		"where is it":  VerdictFollowup,
		"OK，那我们这边测试好了和你说，如果我在 app 发帖是不是就会写到你的 mq 里面": VerdictFollowup,
	} {
		if got := verdictOf([]string{text}); got != want {
			t.Errorf("verdictOf(%q) = %q, want %q", text, got, want)
		}
	}
	if verdictOf(nil) != "" {
		t.Error("silence has a verdict")
	}
}
