package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"vigil/internal/inbox"
)

var approvalChoices = []inbox.Choice{
	{Label: "批这一次", Note: "只批这张，不改白名单"},
	{Label: "不批"},
	{Label: "以后都自动批", Note: "把提交人加进白名单再批"},
}

func addChoiceTodo(t *testing.T, h http.Handler) string {
	t.Helper()
	body, _ := json.Marshal(inbox.Item{TaskID: "slack", Title: "小麦：misc-service 生产发布待审批", Kind: "要审批", Choices: approvalChoices})
	rec, env := do(h, "POST", "/api/todos", string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	return env.Data.(map[string]any)["item"].(map[string]any)["id"].(string)
}

func TestPickingAChoiceTellsTheTaskExactlyWhat(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	h := s.Handler()
	id := addChoiceTodo(t, h)
	rec, _ := do(h, "POST", "/api/todos/"+id+"/answer", `{"mode":"choose","choice":"以后都自动批","text":"他是新来的组长"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("choose: %d %s", rec.Code, rec.Body)
	}
	s.Engine.Wait()
	runs, _ := s.Store.Runs("slack", 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	in := runs[0].Input
	for _, want := range []string{"我选了「以后都自动批」", "把提交人加进白名单再批", "他是新来的组长"} {
		if !strings.Contains(in, want) {
			t.Fatalf("input misses %q:\n%s", want, in)
		}
	}
	if it, _ := s.Inbox.Get(id); it.Status != inbox.StatusAnswered || !strings.Contains(it.Answer, "以后都自动批") {
		t.Fatalf("item = %+v", it)
	}
}

func TestOnlyOfferedChoicesCanBePicked(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	h := s.Handler()
	id := addChoiceTodo(t, h)
	if rec, _ := do(h, "POST", "/api/todos/"+id+"/answer", `{"mode":"choose","choice":"全部批了"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown choice: %d", rec.Code)
	}
	body, _ := json.Marshal(inbox.Item{TaskID: "slack", Title: "小林：问接口"})
	_, env := do(h, "POST", "/api/todos", string(body), true)
	plain := env.Data.(map[string]any)["item"].(map[string]any)["id"].(string)
	if rec, _ := do(h, "POST", "/api/todos/"+plain+"/answer", `{"mode":"choose","choice":"批这一次"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("choice on a to-do without choices: %d", rec.Code)
	}
	bad, _ := json.Marshal(inbox.Item{TaskID: "slack", Title: "x", Choices: []inbox.Choice{{Label: "只有一个"}}})
	if rec, _ := do(h, "POST", "/api/todos", string(bad), true); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid choices accepted: %d", rec.Code)
	}
}

func TestSlackPicksAChoiceByNumberOrLabel(t *testing.T) {
	s, _ := mirrorServer(t)
	s.Mirror = nil
	h := s.Handler()
	for answer, want := range map[string]string{"1": "批这一次", "以后都自动批": "以后都自动批", "2。": "不批"} {
		id := addChoiceTodo(t, h)
		it, _ := s.Inbox.Get(id)
		s.applySlackAnswer(it, answer)
		if got, _ := s.Inbox.Get(id); !strings.Contains(got.Answer, "「"+want+"」") {
			t.Fatalf("%q → %q, want %s", answer, got.Answer, want)
		}
		_, _ = s.Inbox.Close(id, "test") // the next to-do with the same title is a new one
	}
	id := addChoiceTodo(t, h)
	it, _ := s.Inbox.Get(id)
	s.applySlackAnswer(it, "先问问他改了什么")
	if got, _ := s.Inbox.Get(id); got.Answer != "先问问他改了什么" {
		t.Fatalf("free text = %q", got.Answer)
	}
	if card := todoCard(it); !strings.Contains(card, "1. 批这一次") || !strings.Contains(card, "只批这张") {
		t.Fatalf("card:\n%s", card)
	}
	s.Engine.Wait() // the answers started runs; let them finish before the temp dir goes
}
