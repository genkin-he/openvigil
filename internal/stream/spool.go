// Package stream keeps event-stream commands running and spools their output to disk so no event is lost.
package stream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	filePending   = "pending.jsonl"
	fileDelivered = "delivered.jsonl"
	keepDelivered = 2000
)

// Event is one line from a stream command.
type Event struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Line string    `json:"line"`
}

// Spool is a durable queue of events for one task.
type Spool struct {
	dir  string
	mu   sync.Mutex
	last int64
}

var (
	registryMu sync.Mutex
	registry   = map[string]*Spool{}
)

// OpenSpool returns the spool for dir. Every caller in the process shares one instance per directory,
// so appends from the stream source and acks from finished runs are serialised by the same mutex.
func OpenSpool(dir string) (*Spool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("spool path: %w", err)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if s, ok := registry[abs]; ok {
		return s, nil
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	s := &Spool{dir: abs}
	events, err := s.read(filePending)
	if err != nil {
		return nil, err
	}
	if n := len(events); n > 0 {
		s.last = events[n-1].Seq
	}
	registry[abs] = s
	return s, nil
}

// Append durably records a line and returns its event.
func (s *Spool) Append(line string, at time.Time) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := at.UnixNano()
	if seq <= s.last {
		seq = s.last + 1
	}
	ev := Event{Seq: seq, At: at, Line: line}
	data, err := json.Marshal(ev)
	if err != nil {
		return Event{}, fmt.Errorf("encode event: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, filePending), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Event{}, fmt.Errorf("open spool: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return Event{}, fmt.Errorf("append spool: %w", err)
	}
	s.last = seq
	return ev, nil
}

// Pending returns up to max undelivered events, oldest first (max <= 0 means all).
func (s *Spool) Pending(max int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.read(filePending)
	if err != nil {
		return nil, err
	}
	if max > 0 && len(events) > max {
		events = events[:max]
	}
	return events, nil
}

// Ack marks every event with Seq <= upTo as delivered.
func (s *Spool) Ack(upTo int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.read(filePending)
	if err != nil {
		return err
	}
	var keep, done []Event
	for _, ev := range events {
		if ev.Seq <= upTo {
			done = append(done, ev)
		} else {
			keep = append(keep, ev)
		}
	}
	if len(done) == 0 {
		return nil
	}
	delivered, err := s.read(fileDelivered)
	if err != nil {
		return err
	}
	delivered = append(delivered, done...)
	if len(delivered) > keepDelivered {
		delivered = delivered[len(delivered)-keepDelivered:]
	}
	if err := s.write(fileDelivered, delivered); err != nil {
		return err
	}
	return s.write(filePending, keep)
}

func (s *Spool) read(name string) ([]Event, error) {
	f, err := os.Open(filepath.Join(s.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return out, nil
}

func (s *Spool) write(name string, events []Event) error {
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	for _, ev := range events {
		data, err := json.Marshal(ev)
		if err != nil {
			tmp.Close()
			return fmt.Errorf("encode event: %w", err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			tmp.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, name))
}
