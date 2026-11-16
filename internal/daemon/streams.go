package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vigil/internal/exec"
	"vigil/internal/notify"
	"vigil/internal/runner"
	"vigil/internal/store"
	"vigil/internal/stream"
	"vigil/internal/task"
)

const streamFailNotify = 5

// Streams keeps one Source per enabled stream task and turns spooled events into batched runs.
type Streams struct {
	Store    *store.Store
	Engine   *runner.Engine
	Notifier runner.Sender
	Logf     func(string, ...any)
	// Intercept, when set, sees each event line first; a line it takes is not spooled for the agent.
	Intercept func(taskID, line string) bool

	mu      sync.Mutex
	running map[string]streamHandle
	timers  map[string]*time.Timer
}

type streamHandle struct {
	command  string
	cancel   context.CancelFunc
	producer *Producer
}

// Sync starts, restarts or stops sources to match the current tasks.
func (m *Streams) Sync(ctx context.Context) {
	tasks, errs := m.Store.List()
	partial := len(errs) > 0
	want := map[string]task.Task{}
	for _, t := range tasks {
		if t.Enabled && t.Trigger.Kind == task.TriggerStream {
			want[t.ID] = t
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		m.running, m.timers = map[string]streamHandle{}, map[string]*time.Timer{}
	}
	for id, h := range m.running {
		t, ok := want[id]
		if !ok && partial {
			continue // a task file failed to load; keep its stream rather than guess
		}
		if !ok || streamKey(t) != h.command {
			if tm, has := m.timers[id]; has {
				tm.Stop()
				delete(m.timers, id)
			}
			h.cancel()
			if h.producer != nil && (!ok || !sameProducer(h.producer.Spec, t.Trigger.Stream.Producer)) {
				go h.producer.Stop()
			}
			delete(m.running, id)
			m.Logf("stream %s stopped", id)
		}
	}
	for id, t := range want {
		if _, ok := m.running[id]; ok {
			continue
		}
		if err := m.start(ctx, t); err != nil {
			m.Logf("stream %s: %v", id, err)
		}
	}
}

func (m *Streams) start(ctx context.Context, t task.Task) error {
	dir, err := m.Store.SpoolDir(t.ID)
	if err != nil {
		return err
	}
	spool, err := stream.OpenSpool(dir)
	if err != nil {
		return err
	}
	ignore, err := task.IgnoreMatcher(t.Trigger.Stream.Ignore)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	handle := streamHandle{command: streamKey(t), cancel: cancel}
	if spec := t.Trigger.Stream.Producer; spec != nil {
		id := t.ID
		prod := &Producer{Spec: *spec, Dir: exec.ExpandHome(t.Agent.Cwd),
			Logf: func(f string, a ...any) { m.Logf("stream %s: "+f, append([]any{id}, a...)...) },
			OnFail: func(err error, streak int) {
				if m.Notifier != nil {
					_ = m.Notifier.Send(sctx, notify.Message{TaskID: id, Title: "Vigil · 事件生产者起不来：" + id,
						Status: store.StatusFail, Summary: fmt.Sprintf("连续 %d 次：%v", streak, err)}, "")
				}
			}}
		handle.producer = prod
		go prod.Run(sctx)
	}
	m.running[t.ID] = handle
	id, window, maxBatch := t.ID, t.Trigger.Stream.BatchWindow.Std(), t.Trigger.Stream.MaxBatch
	src := stream.Source{
		Command: t.Trigger.Stream.Command,
		Dir:     exec.ExpandHome(t.Agent.Cwd),
		OnLine: func(line string) {
			if m.Intercept != nil && m.Intercept(id, line) {
				return
			}
			if ignore(line) {
				return
			}
			if _, err := spool.Append(line, time.Now()); err != nil {
				m.Logf("spool %s: %v", id, err)
				return
			}
			m.Engine.Hub().Publish(runner.Event{Type: "stream.event", TaskID: id, Text: line})
			m.schedule(id, window, spool, maxBatch)
		},
		OnExit: func(err error, streak int) {
			m.Logf("stream %s exited (%d in a row): %v", id, streak, err)
			if streak == streamFailNotify && m.Notifier != nil {
				_ = m.Notifier.Send(sctx, notify.Message{TaskID: id, Title: "Vigil · 事件流反复退出：" + id,
					Status: store.StatusFail, Summary: fmt.Sprint(err)}, "")
			}
		},
	}
	go src.Run(sctx)
	m.Logf("stream %s started", id)
	if pending, _ := spool.Pending(1); len(pending) > 0 {
		m.Engine.Submit(runner.Request{TaskID: id, Trigger: runner.ByStream})
	}
	return nil
}

// schedule debounces runs: the first event opens a batch window; a full batch runs at once.
func (m *Streams) schedule(id string, window time.Duration, spool *stream.Spool, maxBatch int) {
	if pending, _ := spool.Pending(maxBatch); len(pending) >= maxBatch {
		m.fire(id)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.timers[id]; ok {
		return
	}
	m.timers[id] = time.AfterFunc(window, func() { m.fire(id) })
}

func (m *Streams) fire(id string) {
	m.mu.Lock()
	if tm, ok := m.timers[id]; ok {
		tm.Stop()
		delete(m.timers, id)
	}
	m.mu.Unlock()
	m.Engine.Submit(runner.Request{TaskID: id, Trigger: runner.ByStream})
}

// Kick re-runs stream tasks that still have undelivered events (after failures or restarts).
func (m *Streams) Kick() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		if m.Engine.Busy(id) {
			continue
		}
		dir, err := m.Store.SpoolDir(id)
		if err != nil {
			continue
		}
		if sp, err := stream.OpenSpool(dir); err == nil {
			if p, _ := sp.Pending(1); len(p) > 0 && time.Since(p[0].At) > 2*time.Minute {
				m.Engine.Submit(runner.Request{TaskID: id, Trigger: runner.ByStream})
			}
		}
	}
}

// streamKey identifies a stream's settings; a change restarts the source so new settings apply.
func streamKey(t task.Task) string {
	st := t.Trigger.Stream
	prod := ""
	if st.Producer != nil {
		prod = st.Producer.Command + "\x00" + st.Producer.PIDFile + "\x00" + st.Producer.Match
	}
	return fmt.Sprintf("%s\x00%v\x00%v\x00%d\x00%s", st.Command, st.Ignore, st.BatchWindow, st.MaxBatch, prod)
}

func sameProducer(a task.Producer, b *task.Producer) bool {
	return b != nil && a.Command == b.Command && a.PIDFile == b.PIDFile && a.Match == b.Match
}
