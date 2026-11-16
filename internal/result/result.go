// Package result defines the structured result an agent reports at the end of a run and parses it from text.
package result

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Agent-reported statuses.
const (
	OK       = "ok"
	NeedsYou = "needs_you"
	Fail     = "fail"
)

const maxSummary = 2000

// Result is what an agent reports for one run.
type Result struct {
	Status         string   `json:"status"`
	Summary        string   `json:"summary"`
	Question       string   `json:"question,omitempty"`
	Options        []string `json:"options,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Memory         []string `json:"memory,omitempty"`
}

var fence = regexp.MustCompile("(?s)```json\\s*\\n(.*?)\\n```")

// Validate checks a result and returns a normalised copy.
func Validate(r Result) (Result, error) {
	out := r
	out.Status = strings.ToLower(strings.TrimSpace(r.Status))
	switch out.Status {
	case OK, NeedsYou, Fail:
	case "done", "needs_decision", "needs-you":
		out.Status = map[string]string{"done": OK, "needs_decision": NeedsYou, "needs-you": NeedsYou}[out.Status]
	default:
		return Result{}, fmt.Errorf("unknown status %q", r.Status)
	}
	out.Summary = truncate(strings.TrimSpace(r.Summary), maxSummary)
	if out.Summary == "" {
		return Result{}, errors.New("summary is empty")
	}
	if out.Status == NeedsYou && strings.TrimSpace(out.Question) == "" {
		out.Question = out.Summary
	}
	out.Options = append([]string(nil), r.Options...)
	out.Memory = append([]string(nil), r.Memory...)
	return out, nil
}

// Parse decodes a JSON object reported directly (vigil done --json).
func Parse(data []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return Result{}, fmt.Errorf("decode result: %w", err)
	}
	return Validate(r)
}

// FromText finds the last ```json block in an agent's final reply that is a valid result.
func FromText(text string) (Result, bool) {
	blocks := fence.FindAllStringSubmatch(text, -1)
	for i := len(blocks) - 1; i >= 0; i-- {
		if r, err := Parse([]byte(blocks[i][1])); err == nil {
			return r, true
		}
	}
	return Result{}, false
}

// Fallback builds a result from an unstructured final reply.
func Fallback(status, text string) Result {
	clean := strings.TrimSpace(fence.ReplaceAllString(text, ""))
	if clean == "" {
		clean = "（agent 没有给出文字结果）"
	}
	return Result{Status: status, Summary: truncate(clean, 300)}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
