package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"vigil/internal/proc"
	"vigil/internal/store"
)

// A resident run's process lives in the agent's own Claude Code transcript; the run is the slice of it
// between the run's start and a short tail after it ended (the agent usually wraps up after reporting).
const residentTail = 60 * time.Second

var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// transcriptPath finds a Claude Code session transcript under projectsDir (~/.claude/projects).
func transcriptPath(projectsDir, sessionID string) string {
	if !sessionIDPattern.MatchString(sessionID) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(projectsDir, "*", sessionID+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// window returns the time range of run within runs (newest first): from its start until the end plus
// residentTail, cut at the next run's start.
func window(run store.RunRecord, runs []store.RunRecord) (time.Time, time.Time) {
	end := run.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	end = end.Add(residentTail)
	for _, r := range runs {
		if r.ID != run.ID && r.StartedAt.After(run.StartedAt) && r.StartedAt.Before(end) {
			end = r.StartedAt
		}
	}
	return run.StartedAt.Add(-time.Second), end
}

type transcriptEntry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentPart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// residentTranscript renders the part of a session transcript inside [from, to].
func residentTranscript(path string, from, to time.Time) []LogEntry {
	out := []LogEntry{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	_ = proc.Lines(f, func(line []byte) {
		var e transcriptEntry
		if json.Unmarshal(line, &e) != nil || e.Timestamp.Before(from) || e.Timestamp.After(to) {
			return
		}
		switch e.Type {
		case "user":
			var text string
			if json.Unmarshal(e.Message.Content, &text) == nil {
				if first := firstLine(text); first != "" {
					out = append(out, LogEntry{Kind: "input", Text: clip(first)})
				}
			}
		case "assistant":
			var parts []contentPart
			if json.Unmarshal(e.Message.Content, &parts) != nil {
				return
			}
			for _, p := range parts {
				switch p.Type {
				case "text":
					if strings.TrimSpace(p.Text) != "" {
						out = append(out, LogEntry{Kind: "text", Text: clip(p.Text)})
					}
				case "tool_use":
					out = append(out, LogEntry{Kind: "tool", Text: p.Name + " " + clip(toolSummary(p.Input))})
				}
			}
		}
	})
	if len(out) > maxLogEntries {
		out = out[len(out)-maxLogEntries:]
	}
	return out
}

// firstLine returns the first meaningful line of a prompt, skipping the <pasted_content> wrapper
// Claude Code adds around text pasted in by herdr.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "<pasted_content") || strings.HasPrefix(line, "</pasted_content") {
			continue
		}
		return line
	}
	return ""
}
