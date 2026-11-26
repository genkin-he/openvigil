package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vigil/internal/probe"
	"vigil/internal/slackctl"
	"vigil/internal/workhours"
)

func decodeData[T any](t *testing.T, data any) T {
	t.Helper()
	var out T
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseDue(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	for raw, want := range map[string]time.Duration{"4h": 4 * time.Hour, "2d": 48 * time.Hour, "90m": 90 * time.Minute} {
		got, err := parseDue(raw, now)
		if err != nil || got.Sub(now) != want {
			t.Errorf("%s: %v %v", raw, got, err)
		}
	}
	for _, bad := range []string{"1m", "31d", "soon", "-2h"} {
		if _, err := parseDue(bad, now); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestFollowComesDueOnceNudgesThenHandsToOwner(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	if rec, _ := do(h, "POST", "/api/follows", `{"task":"nope","title":"x","due":"1d"}`, true); rec.Code != 400 {
		t.Fatal("unknown task accepted")
	}
	_, env := do(h, "POST", "/api/follows", `{"task":"slack","title":"等小林确认 sit 数据","who":"小林","ref":"slack:C1:1.0","due":"4h"}`, true)
	f := decodeData[Follow](t, env.Data)
	_, env = do(h, "POST", "/api/follows", `{"task":"slack","title":"重复登记","ref":"slack:C1:1.0","due":"1d"}`, true)
	if again := decodeData[Follow](t, env.Data); again.ID != f.ID {
		t.Fatal("same ref registered twice")
	}

	s.handDue() // not due yet
	now = now.Add(5 * time.Hour)
	s.handDue()
	s.handDue() // handed already: not again within followRecheck
	s.Engine.Wait()
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) != 1 || !strings.Contains(runs[0].Input, "追问一次") || !strings.Contains(runs[0].Input, f.ID) {
		t.Fatalf("runs = %+v", runs)
	}

	// The task nudged: due again tomorrow, and next time it goes to the owner.
	do(h, "POST", "/api/follows/"+f.ID+"/snooze", `{"due":"1d","nudged":true}`, true)
	now = now.Add(25 * time.Hour)
	s.handDue()
	s.Engine.Wait()
	runs, _ = s.Store.Runs("slack", 0)
	if len(runs) != 2 || !strings.Contains(runs[0].Input, "不要再追") {
		t.Fatalf("second hand-off = %+v", runs[0].Input)
	}

	if rec, _ := do(h, "POST", "/api/follows/"+f.ID+"/done", `{}`, true); rec.Code != 400 {
		t.Fatal("done without outcome accepted")
	}
	do(h, "POST", "/api/follows/"+f.ID+"/done", `{"outcome":"无回音，已转待办"}`, true)
	_, env = do(h, "GET", "/api/overview", "", true)
	if open := decodeData[map[string]any](t, env.Data)["follows"].([]any); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
}

func TestFollowGoesToOwnerWhenTaskNeverSettlesIt(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	_, env := do(h, "POST", "/api/follows", `{"task":"slack","title":"等审批","due":"1h"}`, true)
	f := decodeData[Follow](t, env.Data)
	for range followMaxHands + 1 {
		now = now.Add(followRecheck + time.Hour)
		s.handDue()
		s.Engine.Wait() // queued requests for one task merge; let each hand-off run on its own
	}
	if runs, _ := s.Store.Runs("slack", 0); len(runs) != followMaxHands {
		t.Fatalf("hand-offs = %d", len(runs))
	}
	if _, ok, _ := s.Inbox.OpenByRef("follow:" + f.ID); !ok {
		t.Fatal("never escalated to the owner")
	}
}

type fakeThreads struct {
	msgs  []slackctl.Message
	calls int
}

func (f *fakeThreads) Messages(context.Context, string, string) ([]slackctl.Message, error) {
	f.calls++
	return f.msgs, nil
}
func (f *fakeThreads) SelfID(context.Context) (string, error) { return "U0SELF", nil }

func TestFollowHandedEarlyWhenSomeoneRepliesInItsThread(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	now := time.Unix(1794377000, 0)
	s.Now = func() time.Time { return now }
	th := &fakeThreads{msgs: []slackctl.Message{
		{TS: "1794376239.0", User: "U0LIN", Text: "后台缺投票"}, // before the follow: not an answer
		{TS: "1794377963.0", User: "U0SELF", Text: "麻烦验收"}, // ours
	}}
	s.Threads = th
	do(h, "POST", "/api/follows", `{"task":"slack","title":"等小林验收","ref":"slack:C1:1794376239.0","due":"1d"}`, true)
	do(h, "POST", "/api/follows", `{"task":"slack","title":"私聊里等","ref":"slack:D1:1794376239.0","due":"1d"}`, true)

	s.peekFollows(context.Background())
	if th.calls != 1 {
		t.Fatalf("thread reads = %d, want only the channel thread (DMs arrive as events)", th.calls)
	}
	s.Engine.Wait()
	if runs, _ := s.Store.Runs("slack", 0); len(runs) != 0 {
		t.Fatalf("handed without a reply: %+v", runs)
	}

	th.msgs = append(th.msgs, slackctl.Message{TS: "1794379009.0", User: "U0LIN", Text: "ok 没问题"})
	s.peekFollows(context.Background())
	s.peekFollows(context.Background()) // the same reply is not handed twice
	s.Engine.Wait()
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) != 1 || !strings.Contains(runs[0].Input, "ok 没问题") || !strings.Contains(runs[0].Input, "follow done") {
		t.Fatalf("runs = %+v", runs)
	}
}

// fakeProbe settles whatever link it was told is settled.
type fakeProbe struct {
	settled map[string]string // link → outcome
	calls   int
}

func (f *fakeProbe) Check(_ context.Context, link, _ string) (probe.Result, error) {
	f.calls++
	if out, ok := f.settled[link]; ok {
		return probe.Result{Known: true, Settled: true, Outcome: out}, nil
	}
	return probe.Result{Known: strings.Contains(link, "merge_requests")}, nil
}

func TestFollowClosesAsSoonAsWhatItWaitsForIsSettled(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	mr := "https://gitlab.example.com/craft/lb-cms/-/merge_requests/2298"
	p := &fakeProbe{settled: map[string]string{}}
	s.Probe = p
	s.Threads = &fakeThreads{}
	do(h, "POST", "/api/follows", `{"task":"slack","title":"MR 待合","who":"你","link":"`+mr+`","ref":"gitlab:lb-cms:2298","due":"3d"}`, true)
	do(h, "POST", "/api/follows", `{"task":"slack","title":"等小林","ref":"slack:C1:1794376239.0","due":"1d"}`, true)

	s.peekFollows(context.Background())
	if open, _ := s.openFollows(); len(open) != 2 || p.calls != 1 {
		t.Fatalf("open = %d, probe calls = %d: an open MR is no reason to close, and Slack threads are not probed", len(open), p.calls)
	}

	p.settled[mr] = "MR 已合并"
	s.peekFollows(context.Background())
	s.peekFollows(context.Background()) // closed once, handed once
	s.Engine.Wait()
	open, _ := s.openFollows()
	if len(open) != 1 || open[0].Title != "等小林" {
		t.Fatalf("open = %+v", open)
	}
	all, _ := s.follows().list()
	for _, f := range all {
		if f.Title == "MR 待合" && (f.Status != FollowDone || !strings.Contains(f.Outcome, "MR 已合并")) {
			t.Fatalf("follow = %+v", f)
		}
	}
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) != 1 || !strings.Contains(runs[0].Input, "MR 已合并") || strings.Contains(runs[0].Input, "follow done") {
		t.Fatalf("the task gets one wrap-up run, with nothing left to close: %+v", runs)
	}
}

// Chasing people happens on weekdays inside working hours: a due time that lands at night or on a weekend
// moves to the next working morning, and nothing is handed out off hours.
func TestFollowsOnlyChaseInWorkingHours(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	cst := time.FixedZone("CST", 8*3600)
	hours, _ := workhours.Parse("10:00-19:00")
	s.FollowHours = hours
	now := time.Date(2026, 11, 13, 16, 0, 0, 0, cst) // Friday afternoon
	s.Now = func() time.Time { return now }

	_, env := do(h, "POST", "/api/follows", `{"task":"slack","title":"等小林确认","due":"4h"}`, true) // Friday 20:00
	f := decodeData[Follow](t, env.Data)
	monday := time.Date(2026, 11, 16, 10, 0, 0, 0, cst)
	if !f.Due.Equal(monday) {
		t.Fatalf("due = %s, want Monday 10:00", f.Due)
	}

	// An item already due (registered before the rule, or re-handed) still waits for working hours.
	do(h, "POST", "/api/follows", `{"task":"slack","title":"等老王","due":"2h"}`, true) // Friday 18:00, inside
	now = time.Date(2026, 11, 14, 11, 0, 0, 0, cst)                                  // Saturday
	s.handDue()
	s.Engine.Wait()
	if runs, _ := s.Store.Runs("slack", 0); len(runs) != 0 {
		t.Fatalf("chased on Saturday: %+v", runs)
	}
	now = monday.Add(time.Minute)
	s.handDue()
	s.Engine.Wait()
	if runs, _ := s.Store.Runs("slack", 0); len(runs) == 0 {
		t.Fatal("not chased on Monday morning")
	}
}

func TestOwnerCanAskTheTaskToCheckAFollowNow(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	_, env := do(h, "POST", "/api/follows", `{"task":"slack","title":"阿木重跑发布流水线的最后一步","who":"阿木","due":"4h"}`, true)
	f := decodeData[Follow](t, env.Data)

	// Long before it is due, the owner says it may be done: the task checks now.
	rec, env := do(h, "POST", "/api/follows/"+f.ID+"/check", "", true)
	if rec.Code != 200 {
		t.Fatalf("check = %d", rec.Code)
	}
	if got := decodeData[Follow](t, env.Data); !got.Checked.Equal(now) || got.Hands != 0 || !got.Due.Equal(f.Due) {
		t.Fatalf("after check = %+v", got)
	}
	s.Engine.Wait()
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) != 1 || !strings.Contains(runs[0].Input, f.ID) || !strings.Contains(runs[0].Input, "follow done "+f.ID) ||
		!strings.Contains(runs[0].Input, "不要追问") {
		t.Fatalf("runs = %+v", runs)
	}

	// A second click right away is refused; a while later it may check again.
	if rec, _ := do(h, "POST", "/api/follows/"+f.ID+"/check", "", true); rec.Code != 400 {
		t.Fatalf("double check = %d", rec.Code)
	}
	now = now.Add(followCheckGap + time.Second)
	if rec, _ := do(h, "POST", "/api/follows/"+f.ID+"/check", "", true); rec.Code != 200 {
		t.Fatalf("later check = %d", rec.Code)
	}
	s.Engine.Wait()

	do(h, "POST", "/api/follows/"+f.ID+"/done", `{"outcome":"已点"}`, true)
	if rec, _ := do(h, "POST", "/api/follows/"+f.ID+"/check", "", true); rec.Code != 400 {
		t.Fatalf("check on a closed follow = %d", rec.Code)
	}
}

// A paused task drops every run it is handed, so a follow-up of a paused task would sit "checking…" and
// get handed again and again. Checking it now is refused with the reason, and once due it goes straight
// to the owner instead.
func TestAFollowOfAPausedTaskGoesToTheOwnerInsteadOfWaiting(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: cms\nname: CMS 修复\nenabled: false\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	_, env := do(h, "POST", "/api/follows", `{"task":"cms","title":"等 Lee 点部署","who":"Lee","due":"1h"}`, true)
	f := decodeData[Follow](t, env.Data)

	rec, env := do(h, "POST", "/api/follows/"+f.ID+"/check", "", true)
	if rec.Code != 400 || !strings.Contains(env.Error, "暂停") {
		t.Fatalf("check on a paused task = %d %q", rec.Code, env.Error)
	}

	now = now.Add(2 * time.Hour)
	s.handDue()
	s.Engine.Wait()
	if runs, _ := s.Store.Runs("cms", 0); len(runs) != 0 {
		t.Fatalf("handed to a paused task: %+v", runs)
	}
	it, ok, _ := s.Inbox.OpenByRef("follow:" + f.ID)
	if !ok || !strings.Contains(it.Analysis+it.Detail+it.Title, "暂停") {
		t.Fatalf("not with the owner, or no reason: %+v", it)
	}
}
