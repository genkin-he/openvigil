package server

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vigil/internal/proc"
	"vigil/internal/store"
)

// What a past round found out, for replaying it: the agent's words, its tool calls and what they returned.
const (
	evidenceMax       = 12000 // characters kept per round, the newest
	evidenceResultMax = 1500  // characters kept per tool result
)

type evidenceEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type evidencePart struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
}

// runEvidence collects a run's process: its own log for a headless run, else the slice of the resident
// session's transcript. "" when nothing was recorded.
func (s *Server) runEvidence(taskID string, run store.RunRecord) string {
	dir, err := s.Store.TaskDir(taskID)
	if err != nil {
		return ""
	}
	if f, err := os.Open(filepath.Join(dir, "logs", run.ID+".jsonl")); err == nil {
		defer f.Close()
		return evidenceOf(f, time.Time{}, time.Time{})
	}
	if run.SessionID == "" {
		return ""
	}
	path := transcriptPath(s.projectsDir(), run.SessionID)
	if path == "" {
		return ""
	}
	runs, err := s.Store.Runs(taskID, 0)
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	from, to := window(run, runs)
	return evidenceOf(f, from, to)
}

// evidenceOf renders the assistant and tool records of a stream; a zero window keeps every record.
func evidenceOf(r io.Reader, from, to time.Time) string {
	var b strings.Builder
	_ = proc.Lines(r, func(line []byte) {
		var e evidenceEntry
		if json.Unmarshal(line, &e) != nil || (e.Type != "assistant" && e.Type != "user") {
			return
		}
		if !from.IsZero() && (e.Timestamp.Before(from) || e.Timestamp.After(to)) {
			return
		}
		var parts []evidencePart
		if json.Unmarshal(e.Message.Content, &parts) != nil {
			return
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				if t := strings.TrimSpace(p.Text); t != "" {
					b.WriteString("[说明] " + t + "\n")
				}
			case "tool_use":
				b.WriteString("[调用] " + p.Name + " " + clipRunes(oneLine(toolSummary(p.Input)), 300) + "\n")
			case "tool_result":
				b.WriteString("[返回] " + clipRunes(resultText(p.Content), evidenceResultMax) + "\n")
			}
		}
	})
	out := []rune(b.String())
	if len(out) > evidenceMax {
		out = out[len(out)-evidenceMax:]
	}
	return string(out)
}

// resultText reads a tool result's content: a string, or a list of text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, bl := range blocks {
		parts = append(parts, bl.Text)
	}
	return strings.Join(parts, "\n")
}
