package runner

import (
	"strings"
	"testing"

	"vigil/internal/result"
)

func TestABlockedDialogOffersItsOwnAnswersNotTheScreen(t *testing.T) {
	screen := "old output\n────────────────────\n Bash command\n 回报本轮结果\n │ vigil done r1 --json '{}'\n" +
		" Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again for vigil commands\n   3. No\n Esc to cancel\n"
	r := blockedResult(screen)
	if r.Status != result.NeedsYou || strings.Join(r.Options, "|") != "Yes|No" {
		t.Fatalf("options = %q (the dialog's one-off answers, never the screen or a lasting allow)", r.Options)
	}
	if r.Summary != "Bash command：回报本轮结果\nvigil done r1 --json '{}'" {
		t.Fatalf("summary = %q", r.Summary)
	}
	if r.Question != "agent 在等你回答「Do you want to proceed?」" {
		t.Fatalf("question = %q", r.Question)
	}
}
