package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// recordLog is an append-only JSON-lines file of records keyed by id: a later line for the same id
// replaces the earlier one, so an update is an append and the file keeps the history.
type recordLog[T any] struct {
	mu   sync.Mutex
	path string
	id   func(T) string
}

func (l *recordLog[T]) list() ([]T, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readLocked()
}

func (l *recordLog[T]) readLocked() ([]T, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()
	byID := map[string]int{}
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var r T
		if json.Unmarshal(sc.Bytes(), &r) != nil || l.id(r) == "" {
			continue
		}
		if i, ok := byID[l.id(r)]; ok {
			out[i] = r
			continue
		}
		byID[l.id(r)] = len(out)
		out = append(out, r)
	}
	return out, sc.Err()
}

func (l *recordLog[T]) appendLocked(r T) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", l.path, err)
	}
	defer f.Close()
	if torn, err := endsTorn(f); err != nil {
		return err
	} else if torn {
		data = append([]byte{'\n'}, data...) // a crash cut the last line short: start on a fresh one
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	return nil
}

// endsTorn reports whether a non-empty file does not end with a newline.
func endsTorn(f *os.File) (bool, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false, err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil {
		return false, fmt.Errorf("read %s: %w", f.Name(), err)
	}
	return last[0] != '\n', nil
}

func (l *recordLog[T]) add(r T) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(r)
}

// addUnless appends r unless an existing record matches dup, which is returned instead (found=true).
// The check and the append happen under one lock, so two callers cannot both add.
func (l *recordLog[T]) addUnless(r T, dup func(T) bool) (T, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	all, err := l.readLocked()
	if err != nil {
		return r, false, err
	}
	for _, x := range all {
		if l.id(x) == l.id(r) || dup(x) {
			return x, true, nil
		}
	}
	return r, false, l.appendLocked(r)
}

// update applies fn to the record with the given id and appends the result. fn returns an error to refuse.
func (l *recordLog[T]) update(id string, fn func(T) (T, error)) (T, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var zero T
	all, err := l.readLocked()
	if err != nil {
		return zero, err
	}
	for _, r := range all {
		if l.id(r) != id {
			continue
		}
		next, err := fn(r)
		if err != nil {
			return zero, err
		}
		return next, l.appendLocked(next)
	}
	return zero, notFound("没有这条记录：%s", id)
}
