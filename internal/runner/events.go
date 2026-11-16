package runner

import (
	"sync"
	"time"
)

// Event is published to live subscribers (the console's SSE stream).
type Event struct {
	Type   string    `json:"type"` // run.started, run.finished, task.changed, log
	TaskID string    `json:"task_id,omitempty"`
	RunID  string    `json:"run_id,omitempty"`
	Status string    `json:"status,omitempty"`
	Text   string    `json:"text,omitempty"`
	At     time.Time `json:"at"`
}

// Hub fans events out to subscribers; slow subscribers drop events instead of blocking runs.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Subscribe returns a channel of events and a function to stop.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Publish sends ev to every subscriber without blocking.
func (h *Hub) Publish(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
