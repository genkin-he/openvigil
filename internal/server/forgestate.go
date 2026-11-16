package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"vigil/internal/store"
	"vigil/internal/task"
)

// Forge stages shown in the console.
const (
	StageAsking  = "asking"  // still working out the requirement
	StageDrafted = "drafted" // task.yaml applied as a paused draft
	StageTrialed = "trialed" // draft has run at least once
	StageLive    = "live"    // task enabled: forging is done
	StageGone    = "gone"    // the task it made was deleted
)

var forgeNamePattern = regexp.MustCompile(`^forge-[0-9a-f]{6}$`)

type forgeRecord struct {
	Name      string    `json:"name"`
	Idea      string    `json:"idea,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// ForgeStatus is the progress of one forge session.
type ForgeStatus struct {
	Name     string `json:"name"`
	Idea     string `json:"idea,omitempty"`
	Stage    string `json:"stage"`
	TaskID   string `json:"task_id,omitempty"`
	TaskName string `json:"task_name,omitempty"`
}

func (s *Server) forgeDir() string { return filepath.Join(s.Store.Root(), "forges") }

func (s *Server) forgePath(name string) (string, error) {
	if !forgeNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid forge session %q", name)
	}
	return filepath.Join(s.forgeDir(), name+".json"), nil
}

func (s *Server) loadForge(name string) (forgeRecord, error) {
	path, err := s.forgePath(name)
	if err != nil {
		return forgeRecord{}, err
	}
	rec := forgeRecord{Name: name}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil
	}
	if err != nil {
		return rec, fmt.Errorf("read forge record: %w", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("decode forge record: %w", err)
	}
	return rec, nil
}

func (s *Server) saveForge(rec forgeRecord) error {
	path, err := s.forgePath(rec.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.forgeDir(), 0o700); err != nil {
		return fmt.Errorf("create forge dir: %w", err)
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

// linkForge remembers which task a forge session produced.
func (s *Server) linkForge(name, taskID string) error {
	rec, err := s.loadForge(name)
	if err != nil {
		return err
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = s.Now()
	}
	rec.TaskID = taskID
	return s.saveForge(rec)
}

// forgeStatus works out how far a forge session got from the task it produced.
func (s *Server) forgeStatus(name string) (ForgeStatus, error) {
	rec, err := s.loadForge(name)
	if err != nil {
		return ForgeStatus{}, err
	}
	st := ForgeStatus{Name: name, Idea: rec.Idea, Stage: StageAsking, TaskID: rec.TaskID}
	if rec.TaskID == "" {
		return st, nil
	}
	t, err := s.Store.Get(rec.TaskID)
	if errors.Is(err, store.ErrNotFound) {
		st.Stage = StageGone
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.TaskName = t.Name
	st.Stage = stageOf(t, s.Store)
	return st, nil
}

func stageOf(t task.Task, st *store.Store) string {
	if t.Enabled {
		return StageLive
	}
	if runs, _ := st.Runs(t.ID, 1); len(runs) > 0 {
		return StageTrialed
	}
	return StageDrafted
}

func (s *Server) forgeStatusHandler(_ http.ResponseWriter, r *http.Request) (any, error) {
	st, err := s.forgeStatus(r.PathValue("name"))
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return st, nil
}

// forgeClose closes the forge session's workspace in herdr and forgets the session.
func (s *Server) forgeClose(_ http.ResponseWriter, r *http.Request) (any, error) {
	name := r.PathValue("name")
	path, err := s.forgePath(name)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	st, _ := s.forgeStatus(name)
	if err := s.Forge.Close(r.Context(), name); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return st, nil
}
