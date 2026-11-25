package server

import (
	"context"
	"strings"
	"testing"

	"vigil/internal/inbox"
	"vigil/internal/runner"
)

const accountErr = "API Error: This organization can't receive replies from Claude right now. An Admin of this organization can find the next step in the Anthropic Verification Portal."

func answeredTodo(t *testing.T, s *Server, task string) inbox.Item {
	t.Helper()
	it, _, err := s.Inbox.Add(inbox.Item{TaskID: task, Title: "陶然：实盘页少了两笔期权单", Reply: "查了下……"})
	if err != nil {
		t.Fatal(err)
	}
	if it, err = s.Inbox.Answer(it.ID, "先排查下订单消费日志，查清楚再回复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inbox.SetDecision(it.ID, inbox.DecisionInstructed); err != nil {
		t.Fatal(err)
	}
	return it
}

// 11-25 12:34 the owner's instruction went out in a round that hit the account block; its retry failed
// too and the card said 「已交给它处理」 for hours. The failure now shows on the card, and the owner can
// hand the answer over again.
func TestAFailedHandoffShowsOnTheToDoAndCanBeSentAgain(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	it := answeredTodo(t, s, "slack")

	finishRun(t, s, "r1", "slack", "fail", todoInput(it, TodoInstruct, "vigil"), "claude exited: some bug")
	got, _ := s.Inbox.Get(it.ID)
	if !strings.Contains(got.HandoffError, "some bug") {
		t.Fatalf("handoff failure not recorded: %+v", got)
	}

	rec, env := do(h, "POST", "/api/todos/"+it.ID+"/resend", "{}", true)
	if rec.Code != 200 {
		t.Fatalf("resend: %d %s", rec.Code, rec.Body.String())
	}
	_ = env
	s.Engine.Wait()
	got, _ = s.Inbox.Get(it.ID)
	if got.HandoffError != "" || got.Status != inbox.StatusAnswered {
		t.Fatalf("after resend: %+v", got)
	}
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) == 0 || runs[0].Trigger != runner.ByAnswer || !strings.Contains(runs[0].Input, it.ID) ||
		!strings.Contains(runs[0].Input, "查清楚再回复") {
		t.Fatalf("answer not handed over again: %+v", runs)
	}
}

func TestOnlyAFailedRunCarryingAnAnswerMarksTheToDo(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	it := answeredTodo(t, s, "slack")
	finishRun(t, s, "r1", "slack", "ok", todoInput(it, TodoInstruct, "vigil"), "")
	finishRun(t, s, "r2", "slack", "fail", "小林：在吗", "some bug")
	if got, _ := s.Inbox.Get(it.ID); got.HandoffError != "" {
		t.Fatalf("marked without a failed handoff: %+v", got)
	}
}

// The same 11-25 block: 「This organization can't receive replies」 failed 47 rounds in 78 minutes with no
// to-do and nothing re-run afterwards. It is now its own system to-do, and the lost rounds run again once
// a round gets through. The edge probe cannot tell an account block is over, so it does not close it.
func TestAnAccountBlockRaisesItsOwnToDoAndRerunsOnceARunGetsThrough(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	s.net.probe = func(context.Context) bool { return true }

	finishRun(t, s, "r1", "slack", "fail", "同事：改个文案", accountErr)
	acc, ok, _ := s.Inbox.OpenByRef(accountBlockedRef)
	if !ok || acc.TaskID != inbox.SystemTask || !strings.Contains(acc.Title, "账号") {
		t.Fatalf("account to-do = %+v (%v)", acc, ok)
	}
	if _, ok := netTodo(t, s); ok {
		t.Fatal("an account block was reported as a blocked network")
	}
	s.probeNet(context.Background())
	if _, ok, _ := s.Inbox.OpenByRef(accountBlockedRef); !ok {
		t.Fatal("the edge probe closed an account block")
	}

	finishRun(t, s, "r2", "slack", "ok", "小林：在吗", "")
	s.Engine.Wait()
	if _, ok, _ := s.Inbox.OpenByRef(accountBlockedRef); ok {
		t.Fatal("still open after a round got through")
	}
	runs, _ := s.Store.Runs("slack", 0)
	if runs[0].Trigger != runner.ByRetry || !strings.Contains(runs[0].Input, "同事：改个文案") {
		t.Fatalf("lost round not re-run: %+v", runs[0])
	}
}
