package server

import (
	"net/http"
	"time"

	"vigil/internal/health"
	"vigil/internal/inbox"
)

// checkMute is how long a check's to-do stays away after the owner put it away while still failing.
const checkMute = 24 * time.Hour

// RaiseCheck opens (or refreshes) the to-do for a failing login check.
func (s *Server) RaiseCheck(c health.Check, output string) error {
	ref := health.Ref(c.Name)
	return s.raiseSystem(ref, "登录失效："+c.Name, c.Hint, "检查命令的输出：\n"+output)
}

// raiseSystem opens or refreshes a to-do Vigil raises about itself.
func (s *Server) raiseSystem(ref, title, detail, analysis string) error {
	if at, ok := s.muted.Load(ref); ok && s.Now().Sub(at.(time.Time)) < checkMute {
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
	s.muted.Delete(ref)
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
	if it.Ref == netBlockedRef {
		s.netRecovered("你说网络修好了，丢掉的几轮已重跑")
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
