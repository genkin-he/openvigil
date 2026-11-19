// Package server exposes Vigil's HTTP API, live events and the embedded console.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vigil/internal/forge"
	"vigil/internal/health"
	"vigil/internal/inbox"
	"vigil/internal/replay"
	"vigil/internal/runner"
	"vigil/internal/store"
	"vigil/internal/workhours"
)

const maxBody = 2 << 20

// Healer is the part of the healer the API can poke.
type Healer interface {
	Reset(id string)
}

// Server serves the API and console.
type Server struct {
	// Dialogs answers a dialog a resident agent is stuck on; nil leaves those to the agent's session.
	Dialogs DialogAnswerer
	Store   *store.Store
	Engine  *runner.Engine
	Forge   forge.Forge
	Healer  Healer
	Inbox   *inbox.Box
	// VigilBin is how agents call this daemon back (the same string the prompts use).
	VigilBin string
	// RaiseTerminal brings the owner's herdr terminal to the front after a takeover (optional).
	RaiseTerminal func(ctx context.Context) error
	// ClaudeProjects is where Claude Code keeps session transcripts (default ~/.claude/projects).
	ClaudeProjects string
	Token          string
	Addr           string
	Web            fs.FS // console assets (index.html etc.)
	Logf           func(string, ...any)
	Now            func() time.Time
	// Checks reports the latest login checks; RecheckNow asks for a check round at once (both optional).
	Checks     func() map[string]health.Result
	RecheckNow func()
	// Patcher lands the code changes the owner approved; nil when no source_dir is configured.
	Patcher *Patcher
	// FollowHours is when follow-ups may chase people (weekdays, working hours); nil means any time.
	FollowHours *workhours.Hours

	net netBlock // rounds lost to a blocked network, re-run once it is back
	// SelfTest runs the end-to-end self-test (optional).
	SelfTest *SelfTest
	// Threads reads Slack threads so follow-ups waiting in a channel thread see replies early (optional).
	Threads ThreadReader
	// Probe looks up merge requests and tickets follow-ups wait for, so they close once settled (optional).
	Probe FollowProbe
	// Mirror posts to-dos to the owner's Slack self-DM and takes answers from their threads (optional).
	Mirror *TodoMirror

	muteLog  *muteLog // system to-dos the owner put away, kept on disk so a restart does not bring them back
	muteOnce sync.Once
	// Replayer replays past rounds under changed rules, with no tools (see package replay).
	Replayer replay.Runner
	// Evolve wakes the self-evolution task when the owner's feedback arrives (optional).
	Evolve *Evolver
	// Migrator runs the health check and starts a move to another computer (optional).
	Migrator *Migrator

	correct     *corrections
	correctOnce sync.Once
	ledgerLog   *ledgerStore
	ledgerOnce  sync.Once
	replaying   sync.Map // rule suggestion ref → a replay for it is running
	followLog   *followStore
	ownerLog    *ownerReplyLog
	ownerOnce   sync.Once
	afterLog    *aftermathLog
	afterOnce   sync.Once
	followOnce  sync.Once
	bg          sync.WaitGroup
	// Base is the daemon's lifetime; long background work (the self-test) stops with it.
	Base context.Context
}

// background runs fn on its own goroutine, counted so shutdown can wait for it (Wait).
func (s *Server) background(fn func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
}

// Wait blocks until background work (Slack posts, answers from Slack) has finished; call it before the
// engine is drained at shutdown, so an answer taken from Slack still makes it into the saved queue.
func (s *Server) Wait() { s.bg.Wait() }

// Handler builds the HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Inbox == nil {
		// Read s.Now on every call, so a clock swapped in later (tests) reaches the inbox too.
		now := func() time.Time { return s.Now() }
		if box, err := inbox.Open(filepath.Join(s.Store.Root(), "inbox"), now); err == nil {
			s.Inbox = box
		} else if s.Logf != nil {
			s.Logf("inbox unavailable: %v", err)
		}
	}
	mux := http.NewServeMux()
	api := func(pattern string, h func(w http.ResponseWriter, r *http.Request) (any, error)) {
		mux.Handle(pattern, s.auth(jsonHandler(h)))
	}
	api("GET /api/overview", s.overview)
	api("GET /api/tasks/{id}", s.taskDetail)
	api("POST /api/tasks", s.applyTask)
	api("DELETE /api/tasks/{id}", s.removeTask)
	api("POST /api/tasks/{id}/run", s.runTask)
	api("POST /api/tasks/{id}/pause", s.pauseTask)
	api("POST /api/tasks/{id}/resume", s.resumeTask)
	api("POST /api/tasks/{id}/answer", s.answerTask)
	api("POST /api/tasks/{id}/jobs", s.spawnJob)
	api("POST /api/tasks/{id}/open", s.openTask)
	api("POST /api/tasks/{id}/memory", s.appendMemory)
	api("POST /api/tasks/{id}/amend", s.amendRunbook)
	api("PUT /api/tasks/{id}/docs/{doc}", s.putDoc)
	api("POST /api/tasks/{id}/proposals/{name}", s.resolveProposal)
	api("GET /api/tasks/{id}/runs/{run}/log", s.runLog)
	api("POST /api/runs/{run}/done", s.runDone)
	api("GET /api/tasks/{id}/trust", s.taskTrust)
	api("GET /api/todos", s.todoList)
	api("POST /api/todos", s.todoAdd)
	api("POST /api/todos/{id}/answer", s.todoAnswer)
	api("POST /api/todos/{id}/note", s.todoNote)
	api("POST /api/todos/{id}/done", s.todoClose)
	api("POST /api/patches", s.patchAdd)
	api("GET /api/checks", s.checkList)
	api("POST /api/checks/run", s.checkRun)
	api("GET /api/selftest", s.selfTestStatus)
	api("POST /api/selftest", s.selfTestStart)
	api("GET /api/retro", s.retro)
	api("GET /api/follows", s.followList)
	api("POST /api/follows", s.followAdd)
	api("POST /api/follows/{id}/done", s.followDone)
	api("POST /api/follows/{id}/snooze", s.followSnooze)
	api("POST /api/follows/{id}/check", s.followCheck)
	api("GET /api/evolve/signals", s.signals)
	api("GET /api/evolve/ledger", s.ledgerList)
	api("POST /api/evolve/replay", s.replayStart)
	api("GET /api/evolve/replay/{id}", s.replayGet)
	api("POST /api/evolve/ledger", s.ledgerAdd)
	api("POST /api/evolve/ledger/{id}", s.ledgerVerdict)
	api("GET /api/owner", s.ownerGet)
	api("POST /api/owner/principles", s.ownerAppend)
	api("PUT /api/owner", s.ownerPut)
	api("GET /api/drain", s.drainStatus)
	api("POST /api/drain", s.setDrain)
	api("GET /api/migrate/doctor", s.migrateDoctor)
	api("GET /api/migrate/plan", s.migratePlan)
	api("POST /api/migrate/export", s.migrateExport)
	api("GET /api/forge", s.forgeList)
	api("POST /api/forge", s.forgeStart)
	api("POST /api/forge/{name}/say", s.forgeSay)
	api("POST /api/forge/{name}/show", s.forgeShow)
	api("GET /api/forge/{name}/screen", s.forgeScreen)
	api("GET /api/forge/{name}/status", s.forgeStatusHandler)
	api("POST /api/forge/{name}/close", s.forgeClose)
	mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.events)))
	mux.HandleFunc("GET /", s.console)
	return s.guardHost(mux)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// guardHost rejects requests whose Host is not this loopback address (DNS rebinding).
func (s *Server) guardHost(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.Addr)
	allowed := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true, "[::1]:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" && r.URL.Path == "/api/events" {
			got = r.URL.Query().Get("token") // EventSource cannot set headers
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, envelope{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type envelope struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// httpError carries a status code.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &httpError{code: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &httpError{code: http.StatusNotFound, msg: fmt.Sprintf(format, args...)}
}

func jsonHandler(h func(w http.ResponseWriter, r *http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		data, err := h(w, r)
		if err != nil {
			code := http.StatusInternalServerError
			var he *httpError
			switch {
			case errors.As(err, &he):
				code = he.code
			case errors.Is(err, store.ErrNotFound):
				code = http.StatusNotFound
			case errors.Is(err, runner.ErrUnknownRun):
				code = http.StatusConflict
			}
			writeJSON(w, code, envelope{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, envelope{OK: true, Data: data})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return badRequest("read body: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return badRequest("invalid json: %v", err)
	}
	return nil
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch, stop := s.Engine.Hub().Subscribe()
	defer stop()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	fmt.Fprint(w, ": hello\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// console serves the embedded single-page app; index.html gets the API token injected.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	data, err := fs.ReadFile(s.Web, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	if path == "index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data = []byte(strings.Replace(string(data), "__VIGIL_TOKEN__", s.Token, 1))
	} else if strings.HasSuffix(path, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else if strings.HasSuffix(path, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(path, ".svg") {
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	_, _ = w.Write(data)
}

// Serve runs the server until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{Addr: s.Addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", s.Addr, err)
	}
	return nil
}
