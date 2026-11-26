package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"vigil/internal/config"
	"vigil/internal/daemon"
	"vigil/internal/exec"
	"vigil/internal/forge"
	"vigil/internal/health"
	"vigil/internal/herdr"
	"vigil/internal/idle"
	"vigil/internal/inbox"
	"vigil/internal/migrate"
	"vigil/internal/notify"
	"vigil/internal/patch"
	"vigil/internal/probe"
	"vigil/internal/replay"
	"vigil/internal/resident"
	"vigil/internal/runner"
	"vigil/internal/server"
	"vigil/internal/slackctl"
	"vigil/internal/starter"
	"vigil/internal/store"
	"vigil/internal/terminal"
	"vigil/internal/workhours"
	"vigil/web"
)

const (
	tickInterval = 15 * time.Second
	residentPoll = 5 * time.Second
	holdTimeout  = 10 * time.Second
	maxLogSize   = 10 << 20
	firstCheck   = time.Minute // login checks start after the daemon has settled
	selfTestStep = 10 * time.Minute
	selfTestPoll = 10 * time.Second
)

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "vigil"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func openLog(root string) (io.Writer, error) {
	path := filepath.Join(root, "vigil.log")
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	return io.MultiWriter(f, os.Stderr), nil
}

// lockSingleton makes sure only one daemon runs per root.
func lockSingleton(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("另一个 Vigil 守护进程已经在运行")
	}
	return f, nil
}

func cmdDaemon(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := migrate.RefuseMovedOut(cfg.Root); err != nil {
		return err
	}
	lock, err := lockSingleton(cfg.Root)
	if err != nil {
		return err
	}
	defer lock.Close()
	w, err := openLog(cfg.Root)
	if err != nil {
		return err
	}
	logger := log.New(w, "", log.LstdFlags)
	logf := logger.Printf

	st, err := store.New(cfg.Root)
	if err != nil {
		return err
	}
	if seeded, err := starter.Seed(st); err != nil {
		logf("starter tasks: %v", err)
	} else if seeded {
		logf("first start: added the self-evolution task (self-evolve)")
	}
	redo, err := runner.RecoverInterrupted(st, time.Now())
	if err != nil {
		logf("recover interrupted runs: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Agents call back into this daemon through the CLI; pin the data dir so a second Vigil
	// (tests, another environment) never talks to the wrong daemon.
	bin := "VIGIL_HOME=" + shellQuote(cfg.Root) + " " + shellQuote(selfPath())
	vigilSession := herdr.New(cfg.HerdrBin, cfg.HerdrSession)
	fg := forge.Forge{H: herdr.New(cfg.HerdrBin, cfg.UserSession), VigilBin: bin, Root: cfg.Root,
		HerdrBin: cfg.HerdrBin, ResidentSession: cfg.HerdrSession}
	res := resident.Executor{H: vigilSession, Server: vigilSession, Poll: residentPoll, Guard: shellQuote(selfPath())}
	notifier := notify.Notifier{Webhook: cfg.Webhook, Log: logf}
	hub := runner.NewHub()
	engine := runner.New(ctx, runner.Config{
		Store: st, Headless: exec.Headless{Bin: cfg.ClaudeBin}, Resident: res, Notifier: notifier, Hub: hub,
		VigilBin: bin, ConsoleURL: cfg.URL(), MaxConcurrent: cfg.MaxConcurrent, Logf: logf,
		Idle:   func() (time.Duration, error) { return idle.Duration(ctx) },
		Locked: func() (bool, error) { return idle.Locked(ctx) },
		Hold: func(id string) bool {
			hctx, cancel := context.WithTimeout(ctx, holdTimeout)
			defer cancel()
			return fg.Held(hctx, id)
		},
	})
	streams := &daemon.Streams{Store: st, Engine: engine, Notifier: notifier, Logf: logf}
	healer := &daemon.Healer{Store: st, Engine: engine, Session: vigilSession, Resident: res, Notifier: notifier, Logf: logf}

	for _, req := range redo {
		logf("re-running %s, interrupted by the last shutdown", req.TaskID)
		engine.Submit(req)
	}
	queuePath := filepath.Join(cfg.Root, "queue.json")
	queued, err := runner.LoadQueue(queuePath)
	if err != nil {
		logf("queue from the last shutdown: %v", err)
	}
	for _, req := range queued {
		logf("running %s, queued before the last shutdown", req.TaskID)
		engine.Submit(req)
	}
	if err := runner.RemoveQueue(queuePath); err != nil {
		logf("%v", err)
	}
	go daemon.RunScheduler(ctx, st, engine, tickInterval, logf)

	box, err := inbox.Open(filepath.Join(cfg.Root, "inbox"), time.Now)
	if err != nil {
		return err
	}
	srv := &server.Server{Dialogs: res, Store: st, Engine: engine, Forge: fg, Healer: healer, Inbox: box, VigilBin: bin,
		Token: cfg.Token, Addr: cfg.Addr, Web: web.FS, Logf: logf,
		RaiseTerminal: func(ctx context.Context) error {
			return terminal.Raise(ctx, cfg.TerminalApp, cfg.UserSession, terminal.ListProcs)
		}}
	srv.Now = time.Now
	srv.Base = ctx
	if home, err := os.UserHomeDir(); err == nil {
		srv.Migrator = &server.Migrator{Home: home, Bin: selfPath(), Load: config.Load, Start: server.StartDetached}
	}
	srv.Replayer = replay.Runner{Bin: cfg.ClaudeBin, Model: cfg.ReplayModel}
	kick := make(chan struct{}, 1)
	checker := &health.Checker{Checks: cfg.Checks, Raise: srv.RaiseCheck, Resolve: srv.ResolveCheck, Logf: logf, RetryAfter: 20 * time.Second}
	srv.Checks = checker.Last
	srv.RecheckNow = func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	go checker.Loop(ctx, firstCheck, cfg.CheckInterval(), kick)
	if cfg.Slack.Enabled() {
		srv.SelfTest = &server.SelfTest{Task: cfg.Slack.Task, Channel: cfg.Slack.SelfDM, Slack: slackctl.Client{},
			Step: selfTestStep, Poll: selfTestPoll, Every: cfg.Slack.SelfTestInterval(), Path: filepath.Join(cfg.Root, "selftest.json")}
		if err := srv.SelfTest.Load(); err != nil {
			logf("self-test: %v", err)
		}
	}
	if cfg.Slack.Enabled() {
		srv.Threads = slackctl.Client{}
	}
	if strings.TrimSpace(cfg.FollowHours) != "" {
		if hours, err := workhours.Parse(cfg.FollowHours); err != nil {
			logf("follow_hours in config.yaml: %v (follow-ups chase at any time)", err)
		} else {
			srv.FollowHours = hours
		}
	}
	if src := strings.TrimSpace(cfg.SourceDir); src != "" {
		ps := patch.Store{Root: filepath.Join(cfg.Root, "patches")}
		srv.Patcher = &server.Patcher{Store: ps, Land: patch.Applier{Repo: src}.Land, Install: startInstall(src, ps)}
	}
	if prober, err := probe.New(probe.Command, cfg.Probes); err != nil {
		logf("probes in config.yaml: %v (only merge requests are looked up)", err)
		srv.Probe, _ = probe.New(probe.Command, nil)
	} else {
		srv.Probe = prober
	}
	if cfg.Slack.Mirror() {
		srv.Mirror = &server.TodoMirror{Task: cfg.Slack.Task, Channel: cfg.Slack.SelfDM, Slack: slackctl.Client{}}
		streams.Intercept = srv.InterceptSlack
	}
	if cfg.EvolveTask != "" && cfg.EvolveTask != "-" {
		srv.Evolve = &server.Evolver{Task: cfg.EvolveTask, Now: time.Now, Logf: logf,
			Submit: func(task, input string) {
				req := runner.Request{TaskID: cfg.EvolveTask, Trigger: runner.ByFeedback, Input: input}
				if task != "" {
					// One task's round runs in its own lane: feedback on one task merges, different tasks do not.
					req.Job, req.JobTitle = "evolve-"+task, "进化「"+task+"」"
				}
				engine.Submit(req)
			},
			After: func(d time.Duration, f func()) {
				time.AfterFunc(d, func() {
					if ctx.Err() == nil { // feedback waiting at shutdown is picked up by the next daily round
						f()
					}
				})
			}}
	}
	go srv.FollowLoop(ctx)
	go srv.PatchLoop(ctx)
	go srv.NetWatchLoop(ctx)
	go srv.DialogWatchLoop(ctx)
	go srv.HarvestLoop(ctx)
	go daemon.RunHealer(ctx, healer, streams, tickInterval) // after Intercept is set: it starts the streams
	logf("vigil daemon started · console %s · claude %s · herdr %s (session %s)", cfg.URL(), cfg.ClaudeBin, cfg.HerdrBin, cfg.HerdrSession)
	err = srv.Serve(ctx)
	stop()
	srv.Wait() // answers taken from Slack reach the engine before its queue is saved
	engine.Wait()
	if err := engine.SaveQueue(queuePath); err != nil {
		logf("save queue: %v", err)
	}
	logf("vigil daemon stopped")
	return err
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
