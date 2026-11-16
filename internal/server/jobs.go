package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"vigil/internal/runner"
)

// maxJobTitle keeps job titles to one readable line.
const maxJobTitle = 80

type jobBody struct {
	Title string `json:"title"`
	Input string `json:"input"`
}

// spawnJob hands a long piece of work to a background job of the task, so its main run can move on.
func (s *Server) spawnJob(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	var b jobBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	title := oneLine(b.Title)
	input := strings.TrimSpace(b.Input)
	if title == "" || input == "" {
		return nil, badRequest("子任务需要 --title 和 --input（要做什么、来源、做完怎么回报）")
	}
	if n := []rune(title); len(n) > maxJobTitle {
		title = string(n[:maxJobTitle]) + "…"
	}
	if _, err := s.Store.Get(id); err != nil {
		return nil, err
	}
	job, err := newJobID()
	if err != nil {
		return nil, err
	}
	s.Engine.Submit(runner.Request{TaskID: id, Trigger: runner.ByJob, Input: input, Job: job, JobTitle: title})
	s.changed(id, "job")
	return map[string]string{"job": job, "title": title}, nil
}

func newJobID() (string, error) {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("job id: %w", err)
	}
	return "j-" + hex.EncodeToString(buf[:]), nil
}
