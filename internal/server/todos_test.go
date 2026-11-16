package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"vigil/internal/inbox"
)

func addTodo(t *testing.T, h http.Handler, body string) inbox.Item {
	t.Helper()
	rec, _ := do(h, "POST", "/api/todos", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("add todo: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Data struct {
			Item inbox.Item `json:"item"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Data.Item
}

func TestTodoSendAnswerReachesTask(t *testing.T) {
	s, h := newServer(t)
	s.VigilBin = "VIGIL_HOME='/x' '/bin/vigil'"
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)

	if rec, _ := do(h, "POST", "/api/todos", `{"task_id":"nope","title":"x"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown task accepted: %d", rec.Code)
	}
	it := addTodo(t, h, `{"task_id":"slack","title":"小林：要闻不对","ref":"C1:1.2","target":"slack:C1:1.2","reply":"已改好"}`)

	ovRec, _ := do(h, "GET", "/api/overview", "", true)
	var ov struct {
		Data struct {
			Todos []inbox.Item `json:"todos"`
		} `json:"data"`
	}
	_ = json.Unmarshal(ovRec.Body.Bytes(), &ov)
	if len(ov.Data.Todos) != 1 || ov.Data.Todos[0].ID != it.ID {
		t.Fatalf("overview todos = %+v", ov.Data.Todos)
	}

	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/answer", `{"mode":"maybe","text":"x"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", rec.Code)
	}
	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/answer", `{"mode":"send","text":"已改好"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/answer", `{"mode":"send","text":"again"}`, true); rec.Code != http.StatusConflict {
		t.Fatalf("second answer: %d", rec.Code)
	}
	s.Engine.Wait()
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) != 1 {
		t.Fatalf("want one answer run, got %d", len(runs))
	}
	in := runs[0].Input
	for _, want := range []string{it.ID, "一字不改", "已改好", "slack:C1:1.2", "VIGIL_HOME='/x' '/bin/vigil' todo done " + it.ID} {
		if !strings.Contains(in, want) {
			t.Fatalf("run input missing %q:\n%s", want, in)
		}
	}

	// The task could not send it: the item goes back to the owner.
	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/note", `{"analysis":"没权限","reopen":true}`, true); rec.Code != http.StatusOK {
		t.Fatalf("note: %d", rec.Code)
	}
	if got, _ := s.Inbox.Get(it.ID); got.Status != inbox.StatusOpen {
		t.Fatalf("reopen: %+v", got)
	}
	do(h, "POST", "/api/todos/"+it.ID+"/done", `{"outcome":"已发出"}`, true)
	if got, _ := s.Inbox.Get(it.ID); got.Status != inbox.StatusDone || got.Outcome != "已发出" {
		t.Fatalf("done: %+v", got)
	}
}

func TestTodoDismissDoesNotRunTask(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	it := addTodo(t, h, `{"task_id":"slack","title":"x"}`)
	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/answer", `{"mode":"dismiss"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	s.Engine.Wait()
	if runs, _ := s.Store.Runs("slack", 0); len(runs) != 0 {
		t.Fatalf("dismiss must not run the task: %d runs", len(runs))
	}
	if got, _ := s.Inbox.Get(it.ID); got.Status != inbox.StatusDone {
		t.Fatalf("dismissed item = %+v", got)
	}
	if rec, _ := do(h, "POST", "/api/todos/td-000000/answer", `{"mode":"send","text":"x"}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown todo: %d", rec.Code)
	}
}

func TestSpawnJob(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	if rec, _ := do(h, "POST", "/api/tasks/slack/jobs", `{"title":"排查","input":"  "}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty input accepted: %d", rec.Code)
	}
	rec, _ := do(h, "POST", "/api/tasks/slack/jobs", `{"title":"排查缺陷","input":"看 24990896"}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"job":"j-`) {
		t.Fatalf("spawn: %d %s", rec.Code, rec.Body)
	}
	s.Engine.Wait()
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) != 1 || runs[0].JobTitle != "排查缺陷" || runs[0].Input != "看 24990896" {
		t.Fatalf("job run = %+v", runs)
	}
}

func TestOwnerTodoDoneClosesWithoutRunningTask(t *testing.T) {
	// A step only the owner can take (loosening the agent's own permissions): "done" closes it,
	// and must not hand the step back to an agent that is not allowed to do it.
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: self-evolve\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	it := addTodo(t, h, `{"task_id":"self-evolve","title":"放行","ref":"evolve:1:a","owner":true,"reply":"vigil apply -f x.yaml"}`)
	if !it.Owner {
		t.Fatalf("owner flag lost: %+v", it)
	}
	again := addTodo(t, h, `{"task_id":"self-evolve","title":"放行","ref":"evolve:1:a","analysis":"再试一次"}`)
	if !again.Owner {
		t.Fatalf("a refresh without the flag must keep it: %+v", again)
	}
	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/answer", `{"mode":"send","text":"vigil apply -f x.yaml"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("done: %d %s", rec.Code, rec.Body)
	}
	s.Engine.Wait()
	if runs, _ := s.Store.Runs("self-evolve", 0); len(runs) != 0 {
		t.Fatalf("owner step must not run the task: %d runs", len(runs))
	}
	if got, _ := s.Inbox.Get(it.ID); got.Status != inbox.StatusDone || !strings.Contains(got.Outcome, "我已亲手做完") {
		t.Fatalf("owner item = %+v", got)
	}
}

func TestDismissReasonsAreRecorded(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\ntrigger: {kind: manual}\n"), true)
	cases := []struct{ body, want string }{
		{`{"mode":"dismiss","reason":"noise"}`, inbox.DecisionNoise},
		{`{"mode":"dismiss","reason":"handled_elsewhere"}`, inbox.DecisionHandledElsewhere},
		{`{"mode":"dismiss"}`, inbox.DecisionStale},
	}
	for i, c := range cases {
		it := addTodo(t, h, `{"task_id":"slack","kind":"问进度","title":"x`+string(rune('a'+i))+`","ref":"C1:`+string(rune('1'+i))+`"}`)
		if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/answer", c.body, true); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.body, rec.Code, rec.Body)
		}
		if got, _ := s.Inbox.Get(it.ID); got.Decision != c.want {
			t.Fatalf("%s: decision = %q, want %q", c.body, got.Decision, c.want)
		}
	}
	it := addTodo(t, h, `{"task_id":"slack","kind":"问进度","title":"y","ref":"C1:9"}`)
	if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/answer", `{"mode":"dismiss","reason":"whatever"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad reason accepted: %d", rec.Code)
	}
}

func TestAtMostOneQuestionADay(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: evo\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	s.Now = func() time.Time { return now }
	ask := func(title string) int {
		rec, _ := do(h, "POST", "/api/todos", `{"task_id":"evo","kind":"`+askKind+`","title":"`+title+`"}`, true)
		return rec.Code
	}
	if code := ask("你删掉「明天修」是因为不想许诺时间吗？"); code != 200 {
		t.Fatalf("first question: %d", code)
	}
	if code := ask("第二个问题"); code != 400 {
		t.Fatalf("second question the same day: %d", code)
	}
	now = now.Add(24 * time.Hour)
	if code := ask("第二天，上一个还没答"); code != 400 {
		t.Fatalf("question while one is unanswered: %d", code)
	}
	items, _ := s.Inbox.List(false)
	s.Inbox.Close(items[0].ID, "是")
	if code := ask("第二天，上一个已答"); code != 200 {
		t.Fatalf("next day's question: %d", code)
	}
	if rec, _ := do(h, "POST", "/api/todos", `{"task_id":"evo","kind":"别的","title":"普通待办"}`, true); rec.Code != 200 {
		t.Fatal("ordinary to-dos are not limited")
	}
	if card := todoCard(inbox.Item{ID: "td-1", Kind: askKind, Title: "问"}); !strings.Contains(card, "「是」") {
		t.Fatalf("question card does not say how to answer:\n%s", card)
	}
}

func TestTaskHandingBackThreeTimesMakesItTheOwnersStep(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: evo\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	it, _, _ := s.Inbox.Add(inbox.Item{TaskID: "evo", Title: "放行 approve.py", Reply: "改 settings.json"})
	for i := 0; i < inbox.MaxReopens; i++ {
		s.Inbox.Answer(it.ID, "照办")
		if rec, _ := do(h, "POST", "/api/todos/"+it.ID+"/note", `{"analysis":"被拦","reopen":true}`, true); rec.Code != 200 {
			t.Fatalf("note: %d", rec.Code)
		}
	}
	if got, _ := s.Inbox.Get(it.ID); !got.Owner {
		t.Fatalf("after %d hand-backs: %+v", inbox.MaxReopens, got)
	}
}

func TestARetriedQuestionWithTheSameRefIsMerged(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: evo\nname: 自进化\ntrigger: {kind: manual}\n"), true)
	body := `{"task_id":"evo","kind":"` + askKind + `","title":"问","ref":"ask:1"}`
	do(h, "POST", "/api/todos", body, true)
	if rec, _ := do(h, "POST", "/api/todos", body, true); rec.Code != 200 {
		t.Fatalf("retry of the same question: %d", rec.Code)
	}
	if items, _ := s.Inbox.List(false); len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
}
