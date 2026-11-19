package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var runIDPattern = regexp.MustCompile(`^r[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

const (
	maxLogEntries = 400
	maxEntryText  = 1200
)

func readAll(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, badRequest("read body: %v", err)
	}
	return data, nil
}

// LogEntry is one readable step of a headless run transcript.
type LogEntry struct {
	Kind string `json:"kind"` // text, tool, result
	Text string `json:"text"`
}

func (s *Server) runLog(_ http.ResponseWriter, r *http.Request) (any, error) {
	id, run := r.PathValue("id"), r.PathValue("run")
	if !runIDPattern.MatchString(run) {
		return nil, badRequest("invalid run id")
	}
	dir, err := s.Store.TaskDir(id)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	f, err := os.Open(filepath.Join(dir, "logs", run+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return s.residentLog(id, run), nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseTranscript(f), nil
}

// residentLog cuts a resident run's process out of the agent's Claude Code transcript.
func (s *Server) residentLog(taskID, runID string) []LogEntry {
	runs, err := s.Store.Runs(taskID, 0)
	if err != nil {
		return []LogEntry{}
	}
	for _, r := range runs {
		if r.ID != runID || r.SessionID == "" {
			continue
		}
		path := transcriptPath(s.projectsDir(), r.SessionID)
		if path == "" {
			return []LogEntry{}
		}
		from, to := window(r, runs)
		return residentTranscript(path, from, to)
	}
	return []LogEntry{}
}

func (s *Server) projectsDir() string {
	if s.ClaudeProjects != "" {
		return s.ClaudeProjects
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

type rawEvent struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	Message struct {
		Content []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

func parseTranscript(r io.Reader) []LogEntry {
	out := []LogEntry{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		var ev rawEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					out = append(out, LogEntry{Kind: "text", Text: clip(c.Text)})
				case "tool_use":
					out = append(out, LogEntry{Kind: "tool", Text: c.Name + " " + clip(toolSummary(c.Input))})
				}
			}
		case "result":
			out = append(out, LogEntry{Kind: "result", Text: clip(ev.Result)})
		}
	}
	if len(out) > maxLogEntries {
		out = out[len(out)-maxLogEntries:]
	}
	return out
}

func toolSummary(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return string(input)
	}
	for _, k := range []string{"command", "description", "file_path", "pattern", "url", "prompt"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return string(input)
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= maxEntryText {
		return s
	}
	return string(r[:maxEntryText]) + "…"
}

func (s *Server) forgeList(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.Forge.List(r.Context())
}

type ideaBody struct {
	Idea string `json:"idea"`
}

func (s *Server) forgeStart(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b ideaBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	hint := forgeHint
	name, err := s.Forge.Start(r.Context(), b.Idea, func() { hint = s.raise(r, hint) })
	if err != nil {
		return nil, badRequest("%v", err)
	}
	if err := s.saveForge(forgeRecord{Name: name, Idea: strings.TrimSpace(b.Idea), StartedAt: s.Now()}); err != nil {
		s.logf("forge record %s: %v", name, err)
	}
	return map[string]string{"name": name, "hint": hint}, nil
}

const forgeHint = "已在 herdr 打开「Vigil 锻造」，直接在那里和锻造师对话（可以贴图、选选项）；这里会跟着显示进度"

func (s *Server) forgeShow(_ http.ResponseWriter, r *http.Request) (any, error) {
	if err := s.Forge.Show(r.Context(), r.PathValue("name")); err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]string{"hint": s.raise(r, forgeHint)}, nil
}

func (s *Server) forgeSay(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b textBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.Text) == "" {
		return nil, badRequest("message is empty")
	}
	if err := s.Forge.Say(r.Context(), r.PathValue("name"), b.Text); err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]string{"sent": "ok"}, nil
}

func (s *Server) forgeScreen(_ http.ResponseWriter, r *http.Request) (any, error) {
	screen, err := s.Forge.Screen(r.Context(), r.PathValue("name"))
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]string{"screen": screen}, nil
}
