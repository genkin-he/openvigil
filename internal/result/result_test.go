package result

import (
	"strings"
	"testing"
)

func TestFromTextPicksLastValidBlock(t *testing.T) {
	text := "先看示例：\n```json\n{\"status\":\"ok\",\"summary\":\"示例\"}\n```\n做完了。\n```json\n{\"status\":\"needs_you\",\"summary\":\"要不要合并\",\"options\":[\"A: 合并\",\"B: 等等\"],\"recommendation\":\"A\"}\n```\n"
	r, ok := FromText(text)
	if !ok || r.Status != NeedsYou || r.Question != "要不要合并" || len(r.Options) != 2 {
		t.Fatalf("FromText = %+v, %v", r, ok)
	}
}

func TestFromTextSkipsInvalidTrailingBlock(t *testing.T) {
	text := "```json\n{\"status\":\"ok\",\"summary\":\"好了\"}\n```\n```json\n{\"foo\":1}\n```"
	r, ok := FromText(text)
	if !ok || r.Summary != "好了" {
		t.Fatalf("FromText = %+v, %v", r, ok)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		in     Result
		status string
		ok     bool
	}{
		{Result{Status: "OK", Summary: "x"}, OK, true},
		{Result{Status: "done", Summary: "x"}, OK, true},
		{Result{Status: "needs_decision", Summary: "x"}, NeedsYou, true},
		{Result{Status: "weird", Summary: "x"}, "", false},
		{Result{Status: "ok", Summary: "  "}, "", false},
	}
	for _, c := range cases {
		got, err := Validate(c.in)
		if (err == nil) != c.ok || got.Status != c.status {
			t.Fatalf("Validate(%+v) = %+v, %v", c.in, got, err)
		}
	}
}

func TestFallbackStripsJSONAndTruncates(t *testing.T) {
	r := Fallback(OK, strings.Repeat("字", 400)+"\n```json\n{}\n```")
	if len([]rune(r.Summary)) != 301 || strings.Contains(r.Summary, "json") {
		t.Fatalf("Fallback summary = %d runes", len([]rune(r.Summary)))
	}
	if Fallback(Fail, "").Summary == "" {
		t.Fatal("empty fallback summary")
	}
}
