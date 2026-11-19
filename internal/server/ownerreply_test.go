package server

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/slackctl"
)

// threadsByChannel answers thread reads per channel+thread and channel history for DMs.
type threadsByChannel struct {
	threads map[string][]slackctl.Message // "channel:thread" → thread messages
	history map[string][]slackctl.Message // channel → recent history, newest first
	reads   int
}

func (f *threadsByChannel) Messages(_ context.Context, channel, thread string) ([]slackctl.Message, error) {
	f.reads++
	return f.threads[channel+":"+thread], nil
}
func (f *threadsByChannel) SelfID(context.Context) (string, error) { return "U0SELF", nil }
func (f *threadsByChannel) History(_ context.Context, channel string, _ int) ([]slackctl.Message, error) {
	f.reads++
	return f.history[channel], nil
}

// addAt raises a to-do as if at the given Slack time.
func addAt(t *testing.T, s *Server, at float64, it inbox.Item) inbox.Item {
	t.Helper()
	s.Now = func() time.Time { return time.Unix(int64(at), 0) }
	got, _, err := s.Inbox.Add(it)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func samplesByTodo(t *testing.T, s *Server) map[string][]string {
	t.Helper()
	got, err := s.ownerReplies().list()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, r := range got {
		out[r.Todo] = append(out[r.Todo], r.Text)
	}
	return out
}

func TestOwnerRepliesAreHarvestedFromTheTodosThread(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")
	th := &threadsByChannel{threads: map[string][]slackctl.Message{"C1:1794369309.0": {
		{TS: "1794369309.0", User: "U0HUI", Text: "配图能直接用 CDN 吗"},
		{TS: "1794369350.0", User: "U0SELF", Text: "我看看"}, // before the to-do was raised: context, not the answer
		{TS: "1794370166.0", User: "U0SELF", Text: "下班前给你。"},
		{TS: "1794370200.0", User: "U0SELF", Text: "可以直接用。 *发送工具* <@U0BOT>"},   // the agent, not the owner
		{TS: "1794370300.0", User: "U0SELF", Text: "<@U0SELF> ai 回错了，应该是 sit"}, // a correction, recorded elsewhere
		{TS: "1794370350.0", User: "U0SELF", Text: " <@U0SELF> "},              // a bare self-mention
		{TS: "1794370400.0", User: "U0HUI", Text: "好"},
	}}}
	s.Threads = th
	ch := addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Kind: "问接口", Title: "小林：配图",
		Reply: "可以直接用社区 CDN 地址，不用再转存。", Ref: "C1:1794369309.0", Target: "slack:C1:1794369309.0"})
	s.AnswerTodoReason(ch.ID, TodoDismiss, "", inbox.DecisionHandledElsewhere)
	addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Title: "审批单", Target: "deploy:https://x"})                               // not Slack
	addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Title: "自测", Ref: selfTestRef + ":t1", Target: "slack:C1:1794369309.0"}) // self-test

	s.Now = func() time.Time { return time.Unix(1794380000, 0) }
	s.harvestOwnerReplies(context.Background())
	s.harvestOwnerReplies(context.Background()) // a second pass adds nothing

	got := samplesByTodo(t, s)
	if len(got) != 1 || len(got[ch.ID]) != 1 || got[ch.ID][0] != "下班前给你。" {
		t.Fatalf("samples = %v", got)
	}
	all, _ := s.ownerReplies().list()
	if r := all[0]; r.Draft == "" || r.Decision != inbox.DecisionHandledElsewhere || r.Kind != "问接口" || !r.Seen.Equal(time.Unix(1794380000, 0)) {
		t.Fatalf("sample = %+v", r)
	}
	if len(f.timers) != 1 {
		t.Fatal("an owner reply far from the draft did not wake the evolution task")
	}
}

func TestOwnerRepliesInADMGoToTheLatestToDoBeforeThem(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	th := &threadsByChannel{history: map[string][]slackctl.Message{"D2": {
		{TS: "1794375000.0", User: "U0SELF", Text: "明天再说"},
		{TS: "1794374000.0", User: "U0ZHI", Text: "还有个事"},
		{TS: "1794372000.0", User: "U0SELF", Text: "下午三点 call"},
		{TS: "1794371000.0", User: "U0ZHI", Text: "有空没"},
	}}}
	s.Threads = th
	first := addAt(t, s, 1794371010, inbox.Item{TaskID: "slack", Title: "小吴：想 call", Ref: "D2:1794371000.0", Target: "slack:D2:1794371000.0"})
	second := addAt(t, s, 1794374010, inbox.Item{TaskID: "slack", Title: "小吴：还有个事", Ref: "D2:1794374000.0", Target: "slack:D2:1794374000.0"})
	s.Now = func() time.Time { return time.Unix(1794380000, 0) }
	s.harvestOwnerReplies(context.Background())
	got := samplesByTodo(t, s)
	if len(got[first.ID]) != 1 || got[first.ID][0] != "下午三点 call" || len(got[second.ID]) != 1 || got[second.ID][0] != "明天再说" {
		t.Fatalf("samples = %v", got)
	}
	if th.reads != 3 {
		t.Fatalf("reads = %d, want 2 threads + the DM history once", th.reads)
	}
}

func TestOwnerRepliesSkipOldAndLongClosedTodos(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	th := &threadsByChannel{}
	s.Threads = th
	addAt(t, s, 1794369309, inbox.Item{TaskID: "slack", Title: "旧事", Target: "slack:C1:1794369309.0"})
	closed := addAt(t, s, 1794380000, inbox.Item{TaskID: "slack", Title: "早结了", Target: "slack:C2:1794379000.0"})
	s.Inbox.Close(closed.ID, "已处理")
	s.Now = func() time.Time { return time.Unix(1794369309, 0).Add(ownerHarvestWindow + time.Hour) }
	s.harvestOwnerReplies(context.Background())
	if th.reads != 0 {
		t.Fatalf("read %d conversations of old or long-settled to-dos", th.reads)
	}
}

func TestSignalsListOwnerRepliesBySeenTime(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	// Written at 10:00, seen by the harvest only at 10:30, after a round that ended at 10:10.
	s.ownerReplies().add(OwnerReply{Todo: "td-1", TS: "1.0", Text: "x", At: time.Unix(1000, 0), Seen: time.Unix(2800, 0)})
	_, env := do(s.Handler(), "GET", "/api/evolve/signals?since="+url.QueryEscape(time.Unix(1600, 0).Format(time.RFC3339)), "", true)
	var view signalsView
	raw, _ := json.Marshal(env.Data)
	json.Unmarshal(raw, &view)
	if len(view.OwnerReplies) != 1 {
		t.Fatalf("a reply seen after the last round is missing: %+v", view.OwnerReplies)
	}
}

// Once a to-do is settled, what the owner writes in the same DM half an hour later is a new topic, not
// a rewrite of the draft.
func TestOwnerRepliesStopHalfAnHourAfterTheTodoClosed(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	s.Threads = &threadsByChannel{history: map[string][]slackctl.Message{"D2": {
		{TS: "1794375000.0", User: "U0SELF", Text: "另外问一下周报的事"}, // 1h after closing: a new topic
		{TS: "1794372100.0", User: "U0SELF", Text: "下午三点 call"}, // 10 min after closing
		{TS: "1794371000.0", User: "U0ZHI", Text: "有空没"},
	}}}
	it := addAt(t, s, 1794371010, inbox.Item{TaskID: "slack", Title: "小吴：想 call", Ref: "D2:1794371000.0", Target: "slack:D2:1794371000.0"})
	s.Now = func() time.Time { return time.Unix(1794371500, 0) }
	if _, err := s.Inbox.Close(it.ID, "已处理"); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Unix(1794380000, 0) }
	s.harvestOwnerReplies(context.Background())
	if got := samplesByTodo(t, s); len(got[it.ID]) != 1 || got[it.ID][0] != "下午三点 call" {
		t.Fatalf("samples = %v", got)
	}
}

// A draft sent as is already says what the owner wanted; DM messages outside its thread are other talk.
func TestAnAsIsTodoOnlyTakesRepliesInItsThread(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	s.Threads = &threadsByChannel{
		threads: map[string][]slackctl.Message{"D2:1794371000.0": {
			{TS: "1794371000.0", User: "U0ZHI", Text: "有空没"},
			{TS: "1794371200.0", User: "U0SELF", Text: "补一句：带上文档"},
		}},
		history: map[string][]slackctl.Message{"D2": {
			{TS: "1794371300.0", User: "U0SELF", Text: "顺便说下别的事"},
			{TS: "1794371000.0", User: "U0ZHI", Text: "有空没"},
		}},
	}
	it := addAt(t, s, 1794371010, inbox.Item{TaskID: "slack", Title: "小吴：想 call", Reply: "可以，三点", Ref: "D2:1794371000.0", Target: "slack:D2:1794371000.0"})
	if _, err := s.Inbox.SetDecision(it.ID, inbox.DecisionAsIs); err != nil { // sent unchanged
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Unix(1794371400, 0) }
	s.harvestOwnerReplies(context.Background())
	if got := samplesByTodo(t, s); len(got[it.ID]) != 1 || got[it.ID][0] != "补一句：带上文档" {
		t.Fatalf("samples = %v", got)
	}
}

// A to-do raised without a sendable draft keeps the reply the agent would have sent; the owner's own
// reply is set against it, so even kinds the agent may not answer build up a record.
func TestOwnerRepliesAreSetAgainstTheReplyTheAgentKeptToItself(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")
	s.Threads = &threadsByChannel{threads: map[string][]slackctl.Message{"C1:1794369309.0": {
		{TS: "1794369309.0", User: "U0HUI", Text: "这个需求这周能排吗"},
		{TS: "1794370166.0", User: "U0SELF", Text: "这周排不进，下周三开始"},
	}}}
	ch := addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Kind: "要排期", Title: "小林：排期",
		Shadow: "今天就可以开始做", Ref: "C1:1794369309.0", Target: "slack:C1:1794369309.0"})
	s.AnswerTodoReason(ch.ID, TodoDismiss, "", inbox.DecisionHandledElsewhere)

	s.Now = func() time.Time { return time.Unix(1794380000, 0) }
	s.harvestOwnerReplies(context.Background())

	got, _ := s.ownerReplies().list()
	if len(got) != 1 || got[0].Draft != "今天就可以开始做" || !got[0].Shadow {
		t.Fatalf("samples = %+v", got)
	}
	if len(f.timers) != 1 {
		t.Fatal("a hand reply far from the agent's own answer must wake self-evolution")
	}
}

func TestAnOpenTodoTheOwnerAnsweredInSlackClosesItself(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	f := &fakeEvolve{}
	s.Evolve = f.evolver("evo")
	s.Threads = &threadsByChannel{threads: map[string][]slackctl.Message{
		"C1:1794369309.0": {
			{TS: "1794369309.0", User: "U0HUI", Text: "配图能直接用 CDN 吗"},
			{TS: "1794370166.0", User: "U0SELF", Text: "可以，直接用 CDN 地址"},
		},
		"C2:1794369309.0": {
			{TS: "1794369309.0", User: "U0HUI", Text: "帮忙点下审批"},
			{TS: "1794370166.0", User: "U0SELF", Text: "稍等"},
		},
		"C3:1794369309.0": {{TS: "1794369309.0", User: "U0HUI", Text: "还在吗"}}, // no reply from the owner
		"C4:1794369309.0": {
			{TS: "1794369309.0", User: "U0HUI", Text: "这个需求要不要做"},
			{TS: "1794370166.0", User: "U0SELF", Text: "我看看哈"},
		},
	}}
	answered := addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Kind: "问接口", Title: "小林：配图",
		Reply: "可以直接用社区 CDN 地址。", Ref: "C1:1794369309.0", Target: "slack:C1:1794369309.0"})
	byHandOnly := addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Title: "去审批页点通过", Owner: true,
		Ref: "C2:1794369309.0", Target: "slack:C2:1794369309.0"})
	silent := addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Title: "还在吗", Ref: "C3:1794369309.0", Target: "slack:C3:1794369309.0"})

	holding := addAt(t, s, 1794369400, inbox.Item{TaskID: "slack", Kind: "要不要做", Title: "要不要做", Ref: "C4:1794369309.0", Target: "slack:C4:1794369309.0"})

	s.Now = func() time.Time { return time.Unix(1794380000, 0) }
	s.harvestOwnerReplies(context.Background())

	if got, _ := s.Inbox.Get(holding.ID); got.Status != inbox.StatusOpen {
		t.Fatalf("a holding reply closed the to-do: %+v", got)
	}
	got, _ := s.Inbox.Get(answered.ID)
	if got.Status != inbox.StatusDone || got.Decision != inbox.DecisionHandledElsewhere || !strings.Contains(got.Outcome, "可以，直接用 CDN 地址") {
		t.Fatalf("answered to-do = %+v", got)
	}
	// A to-do only the owner can do (approve on a page, say) is not done just because he wrote in the thread.
	if got, _ := s.Inbox.Get(byHandOnly.ID); got.Status != inbox.StatusOpen {
		t.Fatalf("owner-only to-do closed: %+v", got)
	}
	if got, _ := s.Inbox.Get(silent.ID); got.Status != inbox.StatusOpen {
		t.Fatalf("to-do with no owner reply closed: %+v", got)
	}
}
