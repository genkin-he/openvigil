package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A task's memory has two parts. memory.md holds its rules (decisions, corrections, lessons, grants) and
// is read every round, so it stays small. Facts worth keeping (where a table lives, how a command is
// spelled, why a service behaves a certain way) go to knowledge/<topic>.md, listed in the prompt by topic
// and read only when a run needs that topic.
const (
	KnowledgeDir  = "knowledge"
	miscTopic     = "misc"
	maxTopicRunes = 40
	// MemoryCap is the size memory.md should stay under; past it, self-evolution folds rules together.
	MemoryCap = 30 * 1024
)

// topicTag reads a "[topic] fact" note.
var topicTag = regexp.MustCompile(`^\[([^\]\n]{1,80})\]\s*(.+)$`)

// topicJunk is what a topic name may not contain in a file name.
var topicJunk = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)

// KnowledgeTopic is one knowledge file of a task.
type KnowledgeTopic struct {
	Topic   string `json:"topic"`
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
}

// topicSlug turns a topic name into a safe file name ("Social Service" → "social-service").
func topicSlug(name string) string {
	slug := strings.Trim(topicJunk.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-")
	if r := []rune(slug); len(r) > maxTopicRunes {
		slug = strings.Trim(string(r[:maxTopicRunes]), "-")
	}
	if slug == "" {
		return miscTopic
	}
	return slug
}

// inNote is one note on its way in: its date, its text, and where it goes.
type inNote struct {
	date  string
	text  string
	topic string // "" for a rule, which stays in memory.md
}

// splitNote reads an optional leading date and "[topic]" tag; a note that reads as a rule stays a rule.
func splitNote(raw string, at time.Time, forceRule bool) (inNote, bool) {
	n := strings.ReplaceAll(strings.TrimSpace(raw), "\n", " ")
	if n == "" {
		return inNote{}, false
	}
	note := inNote{date: at.Format("2006-01-02")}
	if d := datePrefix.FindString(n); d != "" {
		note.date, n = d, strings.TrimSpace(n[len(d):])
	}
	note.text = n
	if forceRule || ruleNote.MatchString(n) {
		return note, true
	}
	note.topic = miscTopic
	if m := topicTag.FindStringSubmatch(n); m != nil {
		note.topic, note.text = topicSlug(m[1]), strings.TrimSpace(m[2])
	}
	return note, true
}

// appendKnowledge adds facts to their topic files, skipping any already there word for word.
// The caller holds s.mu.
func appendKnowledge(dir string, notes []inNote) error {
	if len(notes) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create knowledge dir: %w", err)
	}
	byTopic := map[string][]inNote{}
	for _, n := range notes {
		byTopic[n.topic] = append(byTopic[n.topic], n)
	}
	for topic, ns := range byTopic {
		path := filepath.Join(dir, topic+".md")
		seen, err := knownFacts(path)
		if err != nil {
			return err
		}
		var b strings.Builder
		for _, n := range ns {
			if seen[n.text] {
				continue
			}
			seen[n.text] = true
			fmt.Fprintf(&b, "- %s %s\n", n.date, n.text)
		}
		if b.Len() == 0 {
			continue
		}
		if err := appendFile(path, []byte(b.String())); err != nil {
			return err
		}
	}
	return nil
}

// knownFacts lists the texts already in a knowledge file, without their dates.
func knownFacts(path string) (map[string]bool, error) {
	seen := map[string]bool{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return seen, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read knowledge: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if m := entryDate.FindStringSubmatch(line); m != nil {
			seen[strings.TrimSpace(m[2])] = true
		}
	}
	return seen, nil
}

// Knowledge lists a task's knowledge topics, largest first.
func (s *Store) Knowledge(id string) ([]KnowledgeTopic, error) {
	dir, err := s.TaskDir(id)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, KnowledgeDir, "*.md"))
	if err != nil {
		return nil, err
	}
	out := []KnowledgeTopic{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read knowledge: %w", err)
		}
		out = append(out, KnowledgeTopic{Topic: strings.TrimSuffix(filepath.Base(f), ".md"),
			Entries: strings.Count("\n"+string(data), "\n- "), Bytes: int64(len(data))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// MemorySize is the size of a task's memory.md (its rules), in bytes.
func (s *Store) MemorySize(id string) (int64, error) {
	path, err := s.taskFile(id, fileMemory)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat memory: %w", err)
	}
	return fi.Size(), nil
}

// ErrNoTopic is returned for a knowledge topic the task does not have.
var ErrNoTopic = errors.New("no such knowledge topic")

// ReadKnowledge returns one knowledge file. Only a name Knowledge lists is accepted, so nothing outside
// the knowledge directory can be read through it.
func (s *Store) ReadKnowledge(id, topic string) (string, error) {
	if topic == "" || topicSlug(topic) != topic {
		return "", ErrNoTopic
	}
	dir, err := s.TaskDir(id)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, KnowledgeDir, topic+".md"))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoTopic
	}
	if err != nil {
		return "", fmt.Errorf("read knowledge: %w", err)
	}
	return string(data), nil
}
