package replay

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestArgsAreToolless(t *testing.T) {
	got := Args("sonnet")
	want := []string{"-p", "--output-format", "json", "--model", "sonnet", "--tools", "",
		"--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence", "--setting-sources", "",
		"--json-schema", decisionSchema}
	if !slices.Equal(got, want) {
		t.Fatalf("Args = %q", got)
	}
	for _, banned := range []string{"--allowedTools", "--mcp-config", "--resume", "--add-dir"} {
		if slices.Contains(got, banned) {
			t.Fatalf("replay must never pass %s", banned)
		}
	}
}

func TestDecideRunsInEmptyTempDir(t *testing.T) {
	var seen string
	r := Runner{Bin: "claude", Model: "sonnet", Run: func(_ context.Context, dir string, args []string, stdin string) ([]byte, error) {
		seen = dir
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("dir %s not empty: %v %v", dir, entries, err)
		}
		if !strings.Contains(stdin, "规则全文") || !strings.Contains(stdin, "当时的输入") {
			t.Fatalf("prompt = %q", stdin)
		}
		return []byte(`{"type":"result","result":"","structured_output":{"action":"reply","text":"已修好","why":"有证据"},"total_cost_usd":0.02}`), nil
	}}
	d, cost, err := r.Decide(context.Background(), "规则全文", Case{RunID: "r1", Input: "当时的输入"})
	if err != nil || !d.Valid || d.Action != ActionReply || d.Text != "已修好" || cost != 0.02 {
		t.Fatalf("Decide = %+v %v %v", d, cost, err)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Fatalf("temp dir %s left behind", seen)
	}
}

func TestParseDecisionRejectsGarbage(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"type":"result","structured_output":{"action":"launch","text":"x"}}`,
		`{"type":"result","result":"no structure"}`,
	} {
		if d, _ := ParseDecision([]byte(raw)); d.Valid {
			t.Fatalf("%s parsed as valid: %+v", raw, d)
		}
	}
	// Some versions put the JSON in result instead of structured_output.
	d, _ := ParseDecision([]byte(`{"type":"result","result":"{\"action\":\"ignore\",\"why\":\"通知\"}"}`))
	if !d.Valid || d.Action != ActionIgnore {
		t.Fatalf("result fallback = %+v", d)
	}
}
