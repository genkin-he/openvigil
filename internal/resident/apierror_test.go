package resident

import (
	"strings"
	"testing"
)

// The chat-watch agent on 11-25 09:34: the turn ended on a blocked network, and the screen was taken for its answer.
const blockedScreen = `  如果发现 runbook 可以写得更好（更快、更稳），在汇报前执行：vigil amend chat-watch -f <新 runbook
  文件> --reason "原因"

⏺ API Error: Request blocked before it reached Anthropic's API (HTTP 403) · the network this request came from isn't allowed: usually an unsupported
  region, or a VPN, proxy or hosting network · signing in again won't change this · check your VPN or network and retry · supported regions:
  https://anthropic.com/supported-countries

────────────────────────────────────────
❯ Once fixed, resend your message
────────────────────────────────────────
  [Opus 5.5] ░░░░░░░░░░ 0%
  ⏵⏵ auto mode on (shift+tab to cycle)`

func TestTurnErrorReadsTheAPIErrorThatEndedTheTurn(t *testing.T) {
	got := TurnError(blockedScreen)
	if !strings.HasPrefix(got, "API Error: Request blocked before it reached Anthropic") || !strings.Contains(got, "supported-countries") {
		t.Fatalf("TurnError = %q", got)
	}
	if strings.Contains(got, "Once fixed") || strings.Contains(got, "\n") {
		t.Fatalf("TurnError kept more than the error: %q", got)
	}
}

func TestTurnErrorIgnoresErrorsTheAgentGotPast(t *testing.T) {
	for name, screen := range map[string]string{
		"retried and answered":            "⏺ API Error: Overloaded\n\n⏺ 已回复小王。\n❯ \n",
		"error only quoted in the prompt": "  对方贴了一句 API Error: Request blocked before it reached Anthropic\n\n⏺ 这是对方网络的问题，已回复。\n❯ \n",
		"plain answer":                    "⏺ 已回复。\n❯ \n",
		"no agent output":                 "  API Error: Request blocked before it reached Anthropic\n❯ \n",
	} {
		if got := TurnError(screen); got != "" {
			t.Errorf("%s: TurnError = %q", name, got)
		}
	}
}
