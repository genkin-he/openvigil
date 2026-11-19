package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// Drain stops (on) or resumes (off) starting new runs. Queued requests stay queued; runs already going
// finish normally. A drain switches itself off after expire, so a restart that never comes (an install
// aborted halfway) cannot leave Vigil stalled. Turning it off also stops holding jobs.
func (e *Engine) Drain(on bool, expire time.Duration) {
	e.mu.Lock()
	e.draining = on
	if !on {
		e.holdJobs = false
	}
	e.drainGen++
	gen := e.drainGen
	e.mu.Unlock()
	e.resumeLanes()
	if on {
		e.expireHold(gen, expire, "drain")
	}
}

// HoldJobs stops (on) or resumes (off) starting background jobs only: rounds that answer people keep
// running while an install waits for long jobs to finish, since a restart would cut those jobs off.
func (e *Engine) HoldJobs(on bool, expire time.Duration) {
	e.mu.Lock()
	e.holdJobs = on
	e.drainGen++
	gen := e.drainGen
	e.mu.Unlock()
	e.resumeLanes()
	if on {
		e.expireHold(gen, expire, "job hold")
	}
}

// expireHold lifts every hold after expire unless a later Drain or HoldJobs call took over.
func (e *Engine) expireHold(gen int, expire time.Duration, what string) {
	if expire <= 0 {
		return
	}
	e.cfg.AfterFunc(expire, func() {
		e.mu.Lock()
		stale := e.drainGen != gen || (!e.draining && !e.holdJobs)
		e.mu.Unlock()
		if !stale {
			e.cfg.Logf("%s not followed by a restart within %s, resuming runs", what, expire)
			e.Drain(false, 0)
		}
	})
}

// resumeLanes starts every idle lane whose queued request is no longer held.
func (e *Engine) resumeLanes() {
	e.mu.Lock()
	var resume []string
	for key, q := range e.qs {
		if q.pending != nil && !q.running && !e.heldLocked(*q.pending) {
			q.running = true
			resume = append(resume, key)
		}
	}
	e.mu.Unlock()
	for _, key := range resume {
		e.wg.Add(1)
		go e.drain(key)
	}
}

// heldLocked reports whether req must wait for a hold to lift. e.mu must be held.
func (e *Engine) heldLocked(req Request) bool {
	return e.draining || e.holdJobs && req.Job != ""
}

func (e *Engine) held(req Request) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.heldLocked(req)
}

// Draining reports whether new runs are held back.
func (e *Engine) Draining() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.draining
}

// HoldingJobs reports whether new background jobs are held back.
func (e *Engine) HoldingJobs() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.holdJobs
}

// Running returns every run in progress, background jobs included, oldest first.
func (e *Engine) Running() []LiveRun {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]LiveRun, 0, len(e.live))
	for key, v := range e.live {
		v.TaskID, _, _ = strings.Cut(key, "#")
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b LiveRun) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}

// Pending returns the requests queued but not started, in lane order.
func (e *Engine) Pending() []Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	keys := make([]string, 0, len(e.qs))
	for key, q := range e.qs {
		if q.pending != nil {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	out := make([]Request, 0, len(keys))
	for _, key := range keys {
		out = append(out, *e.qs[key].pending)
	}
	return out
}

// requeue puts a request taken off a lane back in front of anything submitted since. The calling drain
// goroutine keeps owning the lane (running stays true) and releases it at the top of its loop, so no
// second goroutine can start on the lane in between.
func (e *Engine) requeue(key string, req Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.qs[key]
	if q.pending != nil {
		req = merge(req, *q.pending)
	}
	q.pending = &req
}

// SaveQueue writes the requests still queued at shutdown, so the next start runs them instead of
// losing them. Nothing queued removes the file.
func (e *Engine) SaveQueue(path string) error {
	pending := e.Pending()
	if len(pending) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return fmt.Errorf("encode queue: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("write queue: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write queue: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync queue: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close queue: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadQueue reads the queue file a previous shutdown left behind; RemoveQueue it once the requests are
// submitted, so a crash in between does not lose them.
func LoadQueue(path string) ([]Request, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read queue: %w", err)
	}
	var reqs []Request
	if err := json.Unmarshal(data, &reqs); err != nil {
		_ = os.Rename(path, path+".bad")
		return nil, fmt.Errorf("decode queue (kept as %s.bad): %w", path, err)
	}
	return reqs, nil
}

// RemoveQueue deletes the queue file after its requests were handed to the engine.
func RemoveQueue(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove queue: %w", err)
	}
	return nil
}
