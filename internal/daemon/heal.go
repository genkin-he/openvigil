package daemon

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"strconv"
	"sync"
	"time"

	"vigil/internal/herdr"
	"vigil/internal/notify"
	"vigil/internal/resident"
	"vigil/internal/runner"
	"vigil/internal/store"
	"vigil/internal/task"
)

const (
	restartWindow = time.Hour
	maxRestarts   = 3
)

// HerdrSession is what the healer needs from the herdr client.
type HerdrSession interface {
	Running(ctx context.Context) bool
	StartServer() error
}

// Healer keeps the herdr session up, keeps idle resident agents healthy and prevents idle sleep.
type Healer struct {
	Store    *store.Store
	Engine   *runner.Engine
	Session  HerdrSession
	Resident resident.Executor
	Notifier runner.Sender
	Logf     func(string, ...any)

	mu        sync.Mutex
	restarts  map[string][]time.Time
	gaveUp    map[string]bool
	warned    map[string]bool
	caffeine  *osexec.Cmd
	caffeineD chan struct{}
}

// Check runs one healing pass.
func (h *Healer) Check(ctx context.Context) {
	tasks, _ := h.Store.List()
	var residents []task.Task
	anyEnabled := false
	for _, t := range tasks {
		if !t.Enabled {
			continue
		}
		anyEnabled = true
		if t.Mode == task.ModeResident {
			residents = append(residents, t)
		}
	}
	h.keepAwake(anyEnabled)
	if len(residents) == 0 {
		return
	}
	if !h.Session.Running(ctx) {
		h.Logf("herdr session is down, starting it")
		if err := h.Session.StartServer(); err != nil {
			h.Logf("start herdr session: %v", err)
			return
		}
		time.Sleep(2 * time.Second)
	}
	for _, t := range residents {
		if h.Engine.Busy(t.ID) {
			continue
		}
		h.ensure(ctx, t)
	}
}

func (h *Healer) ensure(ctx context.Context, t task.Task) {
	h.mu.Lock()
	if h.restarts == nil {
		h.restarts, h.gaveUp, h.warned = map[string][]time.Time{}, map[string]bool{}, map[string]bool{}
	}
	if h.gaveUp[t.ID] {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	_, err := h.Resident.H.AgentGet(ctx, task.AgentName(t.ID))
	missing := errors.Is(err, herdr.ErrNoAgent)
	if missing && !h.allowRestart(t.ID) {
		h.notify(ctx, t, "自愈用尽：1 小时内重启 agent 已达 3 次，先停止自愈", "去 herdr 的 vigil 会话看看，处理后执行 vigil resume "+t.ID)
		return
	}
	if missing {
		h.Logf("resident %s missing, starting it", t.ID)
	}
	_, err = h.Resident.Ensure(ctx, t)
	var dialog *resident.UnknownDialogError
	switch {
	case errors.As(err, &dialog):
		h.warnOnce(ctx, t, "agent 停在一个不认识的对话框上", resident.ReadPrompt(dialog.Screen).Text())
	case err != nil:
		h.Logf("ensure %s: %v", t.ID, err)
	default:
		h.mu.Lock()
		delete(h.warned, t.ID)
		h.mu.Unlock()
	}
}

func (h *Healer) allowRestart(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	var recent []time.Time
	for _, at := range h.restarts[id] {
		if now.Sub(at) < restartWindow {
			recent = append(recent, at)
		}
	}
	if len(recent) >= maxRestarts {
		h.gaveUp[id] = true
		h.restarts[id] = recent
		return false
	}
	h.restarts[id] = append(recent, now)
	return true
}

// Reset clears the give-up mark of a task (vigil resume).
func (h *Healer) Reset(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.gaveUp, id)
	delete(h.restarts, id)
	delete(h.warned, id)
}

func (h *Healer) warnOnce(ctx context.Context, t task.Task, summary, detail string) {
	h.mu.Lock()
	seen := h.warned[t.ID]
	h.warned[t.ID] = true
	h.mu.Unlock()
	if !seen {
		h.notify(ctx, t, summary, detail)
	}
}

func (h *Healer) notify(ctx context.Context, t task.Task, summary, detail string) {
	h.Logf("%s: %s", t.ID, summary)
	if h.Notifier == nil {
		return
	}
	_ = h.Notifier.Send(ctx, notify.Message{TaskID: t.ID, Title: "Vigil · " + t.Name + "：需要你", Status: store.StatusNeedsYou,
		Summary: summary, Detail: detail}, t.Notify.Command)
}

// keepAwake holds `caffeinate -i -w <pid>` while any task is enabled.
func (h *Healer) keepAwake(on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	alive := h.caffeine != nil
	if alive {
		select {
		case <-h.caffeineD:
			alive = false
			h.caffeine = nil
		default:
		}
	}
	switch {
	case on && !alive:
		cmd := osexec.Command("/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
		if err := cmd.Start(); err != nil {
			h.Logf("caffeinate: %v", err)
			return
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		h.caffeine, h.caffeineD = cmd, done
	case !on && alive:
		_ = h.caffeine.Process.Kill()
		h.caffeine = nil
	}
}

// RunHealer runs Check every interval until ctx ends.
func RunHealer(ctx context.Context, h *Healer, streams *Streams, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		streams.Sync(ctx)
		streams.Kick()
		h.Check(ctx)
		select {
		case <-ctx.Done():
			h.keepAwake(false)
			return
		case <-t.C:
		}
	}
}
