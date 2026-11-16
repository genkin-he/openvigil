package server

import (
	"net/http"
	"time"

	"vigil/internal/runner"
)

// drainExpire bounds a drain: if no restart follows, runs resume on their own.
const drainExpire = 20 * time.Minute

type drainView struct {
	Draining bool             `json:"draining"`
	Running  []runner.LiveRun `json:"running"`
	Pending  int              `json:"pending"`
}

func (s *Server) drainView() drainView {
	return drainView{Draining: s.Engine.Draining(), Running: s.Engine.Running(), Pending: len(s.Engine.Pending())}
}

// drainStatus tells an installer whether runs are still going.
func (s *Server) drainStatus(_ http.ResponseWriter, _ *http.Request) (any, error) {
	return s.drainView(), nil
}

// setDrain holds back new runs before a restart (on) or lets them go again (off).
func (s *Server) setDrain(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b struct {
		On bool `json:"on"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	s.Engine.Drain(b.On, drainExpire)
	if s.Logf != nil {
		s.Logf("drain %v requested", b.On)
	}
	return s.drainView(), nil
}
