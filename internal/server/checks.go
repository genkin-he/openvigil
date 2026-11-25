package server

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"vigil/internal/health"
	"vigil/internal/inbox"
)

// checkMute is how long a check's to-do stays away after the owner put it away while still failing.
const checkMute = 24 * time.Hour

const (
	mutesFile        = "mutes.jsonl"
	checkReportLines = 8 // lines of a check's output shown on its card
	checkReportLine  = 200
)

// systemMute records when the owner put a system to-do away; a zero At lifts it.
type systemMute struct {
	Ref string    `json:"ref"`
	At  time.Time `json:"at"`
}

type muteLog = recordLog[systemMute]

func (s *Server) mutes() *muteLog {
	s.muteOnce.Do(func() {
		s.muteLog = &muteLog{path: filepath.Join(s.Store.Root(), mutesFile), id: func(m systemMute) string { return m.Ref }}
	})
	return s.muteLog
}

// muted reports whether the owner put the system to-do with this ref away within checkMute.
func (s *Server) muted(ref string) bool {
	all, err := s.mutes().list()
	if err != nil {
		s.logf("mutes: %v", err)
		return false
	}
	for _, m := range all {
		if m.Ref == ref {
			return !m.At.IsZero() && s.Now().Sub(m.At) < checkMute
		}
	}
	return false
}

func (s *Server) setMute(ref string, at time.Time) {
	if err := s.mutes().add(systemMute{Ref: ref, At: at}); err != nil {
		s.logf("mute %s: %v", ref, err)
	}
}

// RaiseCheck opens (or refreshes) the to-do for a failing login check. What the check reported (which
// platform, what to run) heads the card; the check's general hint follows.
func (s *Server) RaiseCheck(c health.Check, output string) error {
	ref := health.Ref(c.Name)
	return s.raiseSystem(ref, "登录失效："+c.Name, checkDetail(output, c.Hint), "检查命令的输出：\n"+output)
}

// exitLine is the「（exit status 1）」the checker appends: it says nothing the owner can act on.
var exitLine = regexp.MustCompile(`^（exit status \d+）$`)

// checkDetail is the card text of a failing check: the first lines of its report, then the hint.
func checkDetail(output, hint string) string {
	var lines []string
	for _, l := range strings.Split(output, "\n") {
		if l = strings.TrimSpace(l); l != "" && !exitLine.MatchString(l) && len(lines) < checkReportLines {
			lines = append(lines, clipRunes(l, checkReportLine))
		}
	}
	if len(lines) == 0 {
		return hint
	}
	return strings.Join(lines, "\n") + "\n\n" + hint
}

// raiseSystem opens or refreshes a to-do Vigil raises about itself.
func (s *Server) raiseSystem(ref, title, detail, analysis string) error {
	if s.muted(ref) {
		return nil // the owner put it away recently
	}
	it, created, err := s.Inbox.Add(inbox.Item{TaskID: inbox.SystemTask, Title: title, Detail: detail, Analysis: analysis, Ref: ref})
	if err != nil {
		return err
	}
	what := "todo.updated" // still failing: the console stops showing "rechecking" and says so
	if created {
		what = "todo.added"
	}
	s.todoChanged(it, what)
	return nil
}

// resolveSystem closes the open system to-do with this ref, if any.
func (s *Server) resolveSystem(ref, outcome string) error {
	it, ok, err := s.Inbox.OpenByRef(ref)
	if err != nil || !ok {
		return err
	}
	closed, err := s.Inbox.Close(it.ID, outcome)
	if err != nil {
		return err
	}
	s.todoChanged(closed, "todo.closed")
	return nil
}

// ResolveCheck closes the to-do of a check that passes again.
func (s *Server) ResolveCheck(c health.Check) error {
	ref := health.Ref(c.Name)
	if s.muted(ref) {
		s.setMute(ref, time.Time{})
	}
	return s.resolveSystem(ref, "已恢复（自检通过）")
}

func (s *Server) recheck() {
	if s.RecheckNow != nil {
		s.RecheckNow()
	}
}

// retrySystem re-runs whatever raised a system to-do: the self-test for its own, the login checks otherwise.
func (s *Server) retrySystem(it inbox.Item) {
	if it.Ref == selfTestRef && s.SelfTest != nil {
		s.startSelfTest(false)
		return
	}
	if it.Ref == netBlockedRef || it.Ref == accountBlockedRef {
		s.netRecovered("你说已经恢复了，丢掉的几轮已重跑")
		return
	}
	s.recheck()
}

func (s *Server) checkList(_ http.ResponseWriter, _ *http.Request) (any, error) {
	if s.Checks == nil {
		return map[string]health.Result{}, nil
	}
	return s.Checks(), nil
}

func (s *Server) checkRun(_ http.ResponseWriter, _ *http.Request) (any, error) {
	s.recheck()
	return map[string]bool{"queued": true}, nil
}
