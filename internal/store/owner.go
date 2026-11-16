package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The owner's principles: how the owner judges things, distilled from their own corrections, replies and
// decisions. Unlike a task's memory it is shared: every task reads it each round.
const fileOwner = "owner.md"

// OwnerPath is where the principles live.
func (s *Store) OwnerPath() string { return filepath.Join(s.root, fileOwner) }

// ReadOwner returns the principles ("" when there are none yet).
func (s *Store) ReadOwner() (string, error) {
	data, err := os.ReadFile(s.OwnerPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", fileOwner, err)
	}
	return string(data), nil
}

// AppendOwner adds one dated principle on a line of its own.
func (s *Store) AppendOwner(line string, at time.Time) error {
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return errors.New("原则不能为空")
	}
	if !datePrefix.MatchString(line) {
		line = at.Format("2006-01-02") + " " + line
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendFile(s.OwnerPath(), []byte("- "+line+"\n"))
}

// WriteOwner replaces the principles, keeping the previous version under history/.
func (s *Store) WriteOwner(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := os.ReadFile(s.OwnerPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", fileOwner, err)
	}
	if len(old) > 0 {
		dir := filepath.Join(s.root, dirHistory)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create history dir: %w", err)
		}
		name := "owner." + time.Now().UTC().Format("20060102T150405.000000000") + ".md"
		if err := writeAtomic(filepath.Join(dir, name), old); err != nil {
			return err
		}
	}
	return writeAtomic(s.OwnerPath(), []byte(content))
}
