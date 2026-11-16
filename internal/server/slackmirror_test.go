package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"vigil/internal/inbox"
)

type fakeMessenger struct {
	mu    sync.Mutex
	posts []string // "thread|text"
	full  map[string]string
}

func (f *fakeMessenger) Post(_ context.Context, _, text, thread string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, thread+"|"+text)
	return "900.1", nil
}

func (f *fakeMessenger) Reply(_ context.Context, _, _, ts string) (string, error) {
	return f.full[ts], nil
}

func (f *fakeMessenger) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		got := append([]string(nil), f.posts...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("posts = %q, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mirrorServer(t *testing.T) (*Server, *fakeMessenger) {
	t.Helper()
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	fm := &fakeMessenger{full: map[string]string{}}
	s.Mirror = &TodoMirror{Task: "slack", Channel: "D1", Slack: fm}
	return s, fm
}

func waitSlackTS(t *testing.T, s *Server, id string) inbox.Item {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		it, _ := s.Inbox.Get(id)
		if it.SlackTS != "" {
			return it
		}
		if time.Now().After(deadline) {
			t.Fatal("card ts never stored")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMirrorPostsCardAndSendsOnReply(t *testing.T) {
	s, fm := mirrorServer(t)
	_, h := s, s.Handler()
	rec, _ := do(h, "POST", "/api/todos", `{"task_id":"slack","title":"小林：问订单","reply":"已经修好了","ref":"C1:1"}`, true)
	if rec.Code != 200 {
		t.Fatalf("add: %s", rec.Body)
	}
	posts := fm.wait(t, 1)
	if !strings.HasPrefix(posts[0], "|【Vigil 待办 td-") || !strings.Contains(posts[0], "> 已经修好了") {
		t.Fatalf("card = %q", posts[0])
	}
	items, _ := s.Inbox.List(false)
	it := waitSlackTS(t, s, items[0].ID)

	// Vigil's own posts are swallowed, unrelated self-DM messages pass through.
	if !s.InterceptSlack("slack", "SLACK D1:900.1 thread=- user=U1 dm=1 mention=0 self=1 | 【Vigil 待办 td-x】…") {
		t.Fatal("card echo reached the agent")
	}
	if s.InterceptSlack("slack", "SLACK D1:901.0 thread=- user=U1 dm=1 mention=0 self=1 | 帮我查下订单") {
		t.Fatal("a normal instruction was taken")
	}
	if s.InterceptSlack("other", "SLACK D1:902.0 thread=900.1 user=U1 dm=1 mention=0 self=1 | 发") {
		t.Fatal("another task's line was taken")
	}

	if !s.InterceptSlack("slack", "SLACK D1:903.0 thread=900.1 user=U1 dm=1 mention=0 self=1 | 发") {
		t.Fatal("reply in the to-do thread not taken")
	}
	posts = fm.wait(t, 2)
	if !strings.HasPrefix(posts[1], "900.1|【Vigil】收到") {
		t.Fatalf("ack = %q", posts[1])
	}
	cur, _ := s.Inbox.Get(it.ID)
	if cur.Status != inbox.StatusAnswered || cur.Answer != "已经修好了" {
		t.Fatalf("item = %+v", cur)
	}
	s.Engine.Wait()

	// Closing posts the outcome in the thread.
	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/done", `{"outcome":"已发出：已经修好了"}`, true); rec.Code != 200 {
		t.Fatalf("done: %s", rec.Body)
	}
	posts = fm.wait(t, 3)
	if posts[2] != "900.1|【Vigil】已结案：已发出：已经修好了" {
		t.Fatalf("closing note = %q", posts[2])
	}
	// Once finished, its thread is an ordinary conversation again.
	if s.InterceptSlack("slack", "SLACK D1:905.0 thread=900.1 user=U1 dm=1 mention=0 self=1 | 顺便把 X 也发了") {
		t.Fatal("a new instruction in a finished to-do's thread was swallowed")
	}
}

func TestSlackAnswerModes(t *testing.T) {
	s, _ := mirrorServer(t)
	cases := []struct {
		reply, text, wantStatus, wantAnswer string
	}{
		{"草稿", "不用管。", inbox.StatusDone, ""},
		{"草稿", "先查下日志再回他", inbox.StatusAnswered, "先查下日志再回他"},
		{"", "发", inbox.StatusAnswered, "发"}, // no draft: taken as an instruction
	}
	for i, c := range cases {
		it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Title: "t", Reply: c.reply, Ref: "r" + string(rune('a'+i))})
		s.applySlackAnswer(it, c.text)
		got, _ := s.Inbox.Get(it.ID)
		if got.Status != c.wantStatus || (c.wantAnswer != "" && got.Answer != c.wantAnswer) {
			t.Fatalf("case %d: %+v", i, got)
		}
	}
	s.Engine.Wait()
}

func TestSlackDoneClosesOwnerStep(t *testing.T) {
	s, _ := mirrorServer(t)
	it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Title: "放行", Owner: true, Reply: "vigil apply -f x.yaml", Ref: "evolve:1:a"})
	if !strings.Contains(todoCard(it), "需要你亲手做") {
		t.Fatalf("card = %q", todoCard(it))
	}
	if ack := s.applySlackAnswer(it, "做完了"); ack != "" {
		t.Fatalf("ack = %q", ack)
	}
	s.Engine.Wait()
	if runs, _ := s.Store.Runs("slack", 0); len(runs) != 0 {
		t.Fatalf("owner step ran the task: %d runs", len(runs))
	}
	if got, _ := s.Inbox.Get(it.ID); got.Status != inbox.StatusDone {
		t.Fatalf("item = %+v", got)
	}
}

func TestSlackDismissWordsMapToReasons(t *testing.T) {
	s, _ := mirrorServer(t)
	for text, want := range map[string]string{
		"不用管": inbox.DecisionStale, "不该找我": inbox.DecisionNoise, "别找我": inbox.DecisionNoise,
		"我处理了": inbox.DecisionHandledElsewhere, "我回了": inbox.DecisionHandledElsewhere, "在别处处理了": inbox.DecisionHandledElsewhere,
	} {
		it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "slack", Kind: "问进度", Title: text, Ref: "C9:" + text})
		s.applySlackAnswer(it, text)
		if got, _ := s.Inbox.Get(it.ID); got.Status != inbox.StatusDone || got.Decision != want {
			t.Fatalf("%q: %+v, want decision %q", text, got, want)
		}
	}
}
