package resident

import "strings"

const (
	outputMark = "⏺" // starts each thing the agent says or does on its screen
	apiErrMark = "API Error:"
)

// TurnError is the API error a turn ended on, read from the agent's screen: its last output is the error
// (blocked network, overload, …), with the wrapped lines joined. "" when the agent's last output is anything
// else, including when an earlier error was followed by a real answer.
func TurnError(screen string) string {
	lines := strings.Split(screen, "\n")
	last := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), outputMark) {
			last = i
		}
	}
	if last < 0 {
		return ""
	}
	first := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[last]), outputMark))
	if !strings.HasPrefix(first, apiErrMark) {
		return ""
	}
	parts := []string{first}
	for _, l := range lines[last+1:] {
		if strings.TrimSpace(l) == "" || !strings.HasPrefix(l, " ") {
			break // a blank line or the input box ends the message
		}
		parts = append(parts, strings.TrimSpace(l))
	}
	return strings.Join(parts, " ")
}
