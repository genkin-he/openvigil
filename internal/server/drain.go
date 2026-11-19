package server

import (
	"net/http"
	"time"

	"vigil/internal/runner"
)

// drainExpire bounds a drain: if no restart follows, runs resume on their own.
const drainExpire = 20 * time.Minute

// jobHoldExpire bounds a hold on background jobs: an install waits up to a job's timeout for them.
const jobHoldExpire = 2*time.Hour + 30*time.Minute

type drainView struct {
	Draining    bool             `json:"draining"`
	HoldingJobs bool             `json:"holding_jobs"`
	Running     []runner.LiveRun `json:"running"`
	Pending     int              `json:"pending"`
}

func (s *Server) drainView() drainView {
	return drainView{Draining: s.Engine.Draining(), HoldingJobs: s.Engine.HoldingJobs(), Running: s.Engine.Running(), Pending: len(s.Engine.Pending())}
}

// drainStatus tells an installer whether runs are still going.
func (s *Server) drainStatus(_ http.ResponseWriter, _ *http.Request) (any, error) {
	return s.drainView(), nil
}

// setDrain holds back new runs before a restart (on) or lets them go again (off).
func (s *Server) setDrain(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b struct {
		On   bool `json:"on"`
		Jobs bool `json:"jobs"` // hold only background jobs: an install waiting for them to finish
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.On && b.Jobs {
		s.Engine.HoldJobs(true, jobHoldExpire)
	} else {
		s.Engine.Drain(b.On, drainExpire)
	}
	if s.Logf != nil {
		s.Logf("drain %v (jobs only: %v) requested", b.On, b.Jobs)
	}
	return s.drainView(), nil
}
