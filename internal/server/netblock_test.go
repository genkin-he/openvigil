package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/runner"
	"vigil/internal/store"
)

const blockedErr = `claude exited: exit status 1: API Error: 403 {"error":{"type":"forbidden","message":"Request blocked before it reached Anthropic's API"},"api_error_status":"network_edge_blocked"}`

func finishRun(t *testing.T, s *Server, id, task, status, input, errText string) {
	t.Helper()
	rec := store.RunRecord{ID: id, TaskID: task, Trigger: runner.ByStream, Status: status, Input: input, Error: errText,
		StartedAt: time.Unix(100, 0), EndedAt: time.Unix(110, 0)}
	if err := s.Store.AppendRun(rec); err != nil {
		t.Fatal(err)
	}
	s.noteRunForNet(runner.Event{Type: "run.finished", TaskID: task, RunID: id, Status: status})
}

func netTodo(t *testing.T, s *Server) (inbox.Item, bool) {
	t.Helper()
	it, ok, err := s.Inbox.OpenByRef(netBlockedRef)
	if err != nil {
		t.Fatal(err)
	}
	return it, ok
}

func TestABlockedNetworkRaisesOneTodoAndRerunsWhenItRecovers(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)

	finishRun(t, s, "r1", "slack", "fail", "小林：在吗", "claude exited: exit status 1: some bug")
	if _, ok := netTodo(t, s); ok {
		t.Fatal("an ordinary failure was taken for a blocked network")
	}
	finishRun(t, s, "r2", "slack", "fail", "小林：在吗", blockedErr)
	finishRun(t, s, "r3", "slack", "fail", "小周：sitemap", blockedErr)
	finishRun(t, s, "r4", "slack", "fail", "小周：sitemap", blockedErr) // the engine's own retry of r3
	it, ok := netTodo(t, s)
	if !ok || it.TaskID != inbox.SystemTask || !strings.Contains(it.Analysis, "2 轮") {
		t.Fatalf("todo = %+v (%v)", it, ok)
	}

	// The engine's retry of r2 got through: that input is done, only r3's is run again.
	finishRun(t, s, "r5", "slack", "ok", "小林：在吗", "")
	s.Engine.Wait()
	if _, ok := netTodo(t, s); ok {
		t.Fatal("still open after the network came back")
	}
	runs, _ := s.Store.Runs("slack", 0)
	var reruns []string
	for _, r := range runs {
		if r.Trigger == runner.ByRetry {
			reruns = append(reruns, r.Input)
		}
	}
	if len(reruns) != 1 || !strings.Contains(reruns[0], "小周：sitemap") || !strings.Contains(reruns[0], "别重复") {
		t.Fatalf("reruns = %q", reruns)
	}
}

func TestRecheckingTheNetworkTodoRerunsAtOnce(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	finishRun(t, s, "r1", "slack", "fail", "小周：sitemap", blockedErr)
	it, _ := netTodo(t, s)
	if _, err := s.AnswerTodo(it.ID, TodoInstruct, ""); err != nil {
		t.Fatal(err)
	}
	s.Engine.Wait()
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) < 2 {
		t.Fatalf("not rerun: %+v", runs)
	}
}

// Nothing else may run for hours while the network is blocked; a probe that sees the edge let
// requests through again re-runs the lost rounds without waiting for the owner to click 复查.
func TestAProbeThatGetsThroughRerunsTheLostRounds(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: brief\nname: Brief\nenabled: true\ntrigger: {kind: manual}\n"), true)

	reachable := false
	s.net.probe = func(context.Context) bool { return reachable }
	s.probeNet(context.Background()) // nothing lost: nothing to do
	finishRun(t, s, "r1", "brief", "fail", "早报", blockedErr)

	s.probeNet(context.Background())
	if _, ok := netTodo(t, s); !ok {
		t.Fatal("closed while the edge still blocks")
	}
	reachable = true
	s.probeNet(context.Background())
	s.Engine.Wait()
	if _, ok := netTodo(t, s); ok {
		t.Fatal("still open after the probe got through")
	}
	runs, _ := s.Store.Runs("brief", 0)
	if len(runs) < 2 || runs[0].Trigger != runner.ByRetry || !strings.Contains(runs[0].Input, "早报") {
		t.Fatalf("lost round not re-run: %+v", runs)
	}
}

func TestAnthropicReachableReadsTheEdgeAnswer(t *testing.T) {
	cases := []struct {
		code int
		body string
		want bool
	}{
		{401, `{"type":"error","error":{"type":"authentication_error"}}`, true},
		{403, `{"error":{"message":"Request blocked before it reached Anthropic's API"},"api_error_status":"network_edge_blocked"}`, false},
		{403, `<title>Attention Required! | Cloudflare</title>`, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		if got := edgeReachable(context.Background(), srv.URL); got != c.want {
			t.Errorf("%d %q: got %v", c.code, c.body, got)
		}
		srv.Close()
	}
	if edgeReachable(context.Background(), "http://127.0.0.1:1") {
		t.Error("an unreachable host counted as reachable")
	}
}
