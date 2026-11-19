package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ArchiveFile holds a task's older routine memory: still searchable, no longer read every round.
const ArchiveFile = "memory.archive.md"

// memoryKeepDays is how long a routine note (a thread's status, a fact found while digging) stays in
// memory.md before it moves to the archive. Rules never move.
const memoryKeepDays = 3

// archivePointer is the line that tells the agent where older notes went; it is written once, on top.
var archivePointer = "> 超过 " + fmt.Sprint(memoryKeepDays) + " 天的事务流水已自动挪到同目录的 " + ArchiveFile +
	"（不删，遇到相关的人、线程、服务时先 grep 它）；决策、纠错、教训、授权一直留在这里。\n"

// ruleNote marks a memory entry that stays in memory.md however old: decisions, corrections, lessons,
// grants and their withdrawal.
var ruleNote = regexp.MustCompile(`^(决策|纠错|教训|规则|原则|收回)|授权|纠错：|教训：`)

// entryDate reads "- 2026-11-02 …" and returns the date; ok is false for an undated or non-bullet line.
var entryDate = regexp.MustCompile(`^- (\d{4}-\d{2}-\d{2})\s+(.*)`)

// archiveOldMemory moves routine entries older than memoryKeepDays from memory.md to the archive.
// The caller holds s.mu.
func archiveOldMemory(path string, now time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read memory: %w", err)
	}
	cutoff := now.AddDate(0, 0, -memoryKeepDays).Format("2006-01-02")
	var kept, moved strings.Builder
	moving := false // continuation lines follow their entry
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "- ") || !strings.HasPrefix(line, " ") {
			m := entryDate.FindStringSubmatch(strings.TrimRight(line, "\n"))
			moving = m != nil && m[1] < cutoff && !ruleNote.MatchString(m[2])
		}
		if moving {
			moved.WriteString(line)
		} else if line != archivePointer {
			kept.WriteString(line)
		}
	}
	if moved.Len() == 0 {
		return nil
	}
	if err := appendFile(filepath.Join(filepath.Dir(path), ArchiveFile), []byte(moved.String())); err != nil {
		return err
	}
	return writeAtomic(path, []byte(archivePointer+kept.String()))
}
