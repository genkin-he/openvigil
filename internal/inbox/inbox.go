// Package inbox keeps the owner's to-dos that agents raise: a question someone asked that needs the owner's
// call, a drafted reply waiting for a yes, a step that got stuck. Each item lives in its own JSON file.
package inbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Item states.
const (
	StatusOpen     = "open"     // waiting for the owner
	StatusAnswered = "answered" // the owner answered; the owning task is carrying it out
	StatusDone     = "done"     // finished: reply sent, handled elsewhere, or dismissed
)

// ErrNotFound is returned for an unknown item id.
var ErrNotFound = errors.New("待办不存在")

// MaxReopens is how many times a task may hand an answered item back undone before the item becomes the
// owner's own step: past that, running the task again only goes round in circles.
const MaxReopens = 3

const ownerStepNote = "AI 已经来回 %d 次都没办成，改为需要你亲手做：做完点「我已做完」，不做就点「不用管」；再交给 AI 也只会原样退回。"

// ErrClosed is returned when answering an item that is no longer open.
var ErrClosed = errors.New("这条待办已经处理过了")

var idPattern = regexp.MustCompile(`^td-[0-9a-f]{6}$`)

// Item is one to-do.
type Item struct {
	ID       string `json:"id"`
	TaskID   string `json:"task_id"`            // the task that carries out the owner's answer
	Title    string `json:"title"`              // who and what, one line
	Detail   string `json:"detail,omitempty"`   // channel / background
	Analysis string `json:"analysis,omitempty"` // what was checked, the conclusion, what the owner must decide
	Reply    string `json:"reply,omitempty"`    // a drafted reply the owner can send as is
	Shadow   string `json:"shadow,omitempty"`   // with no Reply: what the agent would have sent; never offered for sending, only compared with what the owner wrote
	Link     string `json:"link,omitempty"`     // where the matter lives (Slack deep link, issue, MR …)
	Ref      string `json:"ref,omitempty"`      // stable key of the source, e.g. <channel>:<ts>[:<thread_ts>]; dedupes items
	Target   string `json:"target,omitempty"`   // where a reply goes, e.g. slack:<channel>:<thread_ts> or an issue URL
	Owner    bool   `json:"owner,omitempty"`    // only the owner can take this step (e.g. loosen an agent's permissions); Reply says how
	RunID    string `json:"run_id,omitempty"`   // the run that raised it, for replaying that round later
	Reopens  int    `json:"reopens,omitempty"`  // times the task handed the owner's answer back undone
	Status   string `json:"status"`
	Answer   string `json:"answer,omitempty"` // what the owner decided
	Outcome  string `json:"outcome,omitempty"`
	// TaskClosed marks an item its own task closed (the other side decided, the matter moved on) rather
	// than the owner: the owner may still answer it, and their answer wins.
	TaskClosed bool     `json:"task_closed,omitempty"`
	Kind       string   `json:"kind,omitempty"`     // a short, stable category the raising task gives, e.g.「问进度」; decisions are learned per kind
	Choices    []Choice `json:"choices,omitempty"`  // fixed answers the owner picks from (buttons); free text stays possible
	Decision   string   `json:"decision,omitempty"` // how the owner settled it: as_is, edited, instructed, dismissed
	SlackTS    string   `json:"slack_ts,omitempty"` // the copy posted to the owner's self-DM; replies in its thread answer the item
	// HandoffError is why the round carrying the owner's answer failed; empty while it is on its way.
	HandoffError string `json:"handoff_error,omitempty"`
	// Failure is why the owner's last approved action (say, applying a patch) did not go through; shown on the
	// card until the owner answers again.
	Failure string    `json:"failure,omitempty"`
	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`
}

// Choice is one fixed answer to an item: a short label (the button) and what picking it leads to.
type Choice struct {
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
}

// Limits on choices: a handful of short buttons, not a menu.
const (
	MinChoices     = 2
	MaxChoices     = 5
	MaxChoiceLabel = 30
	MaxChoiceNote  = 120
)

// ValidChoices checks a set of choices; none at all is valid.
func ValidChoices(cs []Choice) error {
	if len(cs) == 0 {
		return nil
	}
	if len(cs) < MinChoices || len(cs) > MaxChoices {
		return fmt.Errorf("选项要 %d 到 %d 个", MinChoices, MaxChoices)
	}
	seen := map[string]bool{}
	for _, c := range cs {
		label := strings.TrimSpace(c.Label)
		switch {
		case label == "":
			return errors.New("选项不能是空的")
		case utf8.RuneCountInString(label) > MaxChoiceLabel:
			return fmt.Errorf("选项「%s」太长：按钮上的字不超过 %d 个，后果写进说明", label, MaxChoiceLabel)
		case utf8.RuneCountInString(c.Note) > MaxChoiceNote:
			return fmt.Errorf("选项「%s」的说明超过 %d 字", label, MaxChoiceNote)
		case isNumber(label):
			return fmt.Errorf("选项「%s」不能只是数字：Slack 里按编号回答会和它混淆", label)
		case seen[label]:
			return fmt.Errorf("选项「%s」重复了", label)
		}
		seen[label] = true
	}
	return nil
}

// ChoiceByLabel finds the choice with this label (surrounding spaces and letter case ignored).
func (it Item) ChoiceByLabel(label string) (Choice, bool) {
	label = strings.TrimSpace(label)
	for _, c := range it.Choices {
		if strings.EqualFold(strings.TrimSpace(c.Label), label) {
			return c, true
		}
	}
	return Choice{}, false
}

func isNumber(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// Box stores items under dir.
type Box struct {
	dir string
	now func() time.Time
	mu  sync.Mutex
}

// Open returns the inbox at dir, creating it when needed.
func Open(dir string, now func() time.Time) (*Box, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create inbox dir: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	return &Box{dir: dir, now: now}, nil
}

// Add creates an item, or, when an unfinished item with the same ref exists, updates that one instead.
// It reports whether a new item was created.
func (b *Box) Add(in Item) (Item, bool, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.TaskID = strings.TrimSpace(in.TaskID)
	if in.Title == "" || in.TaskID == "" {
		return Item{}, false, errors.New("待办需要 title 和 task")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if in.Ref != "" {
		items, err := b.load()
		if err != nil {
			return Item{}, false, err
		}
		for _, it := range items {
			if it.Ref == in.Ref && it.Status != StatusDone {
				merged := mergeInto(it, in, now)
				return merged, false, b.save(merged)
			}
		}
	}
	id, err := newID()
	if err != nil {
		return Item{}, false, err
	}
	it := in
	it.ID, it.Status, it.Answer, it.Outcome, it.Created, it.Updated = id, StatusOpen, "", "", now, now
	return it, true, b.save(it)
}

// mergeInto refreshes an existing item with newer findings, keeping what the newer report leaves empty.
func mergeInto(old, in Item, now time.Time) Item {
	out := old
	pick := func(cur, next string) string {
		if strings.TrimSpace(next) != "" {
			return next
		}
		return cur
	}
	out.Title = pick(old.Title, in.Title)
	out.Detail = pick(old.Detail, in.Detail)
	out.Analysis = pick(old.Analysis, in.Analysis)
	out.Reply = pick(old.Reply, in.Reply)
	out.Shadow = pick(old.Shadow, in.Shadow)
	out.Link = pick(old.Link, in.Link)
	out.Target = pick(old.Target, in.Target)
	out.Kind = pick(old.Kind, in.Kind)
	if len(in.Choices) > 0 {
		out.Choices = in.Choices
	}
	out.RunID = pick(old.RunID, in.RunID) // the first round that raised it stays the source; a refresh does not move it
	out.Owner = old.Owner || in.Owner     // once a step is known to need the owner's hands, a refresh does not undo that
	out.Updated = now
	return out
}

// SystemTask owns the to-dos Vigil raises about itself (expired logins and the like); answering
// one runs no task, it is resolved by Vigil once the problem is gone.
const SystemTask = "vigil"

// OpenByRef returns the unfinished item with this ref, if any.
func (b *Box) OpenByRef(ref string) (Item, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items, err := b.load()
	if err != nil {
		return Item{}, false, err
	}
	for _, it := range items {
		if it.Ref == ref && it.Status != StatusDone {
			return it, true, nil
		}
	}
	return Item{}, false, nil
}

// How the owner settled an item, recorded so repeated identical decisions can become rules.
const (
	DecisionAsIs       = "as_is"      // sent the drafted reply unchanged
	DecisionEdited     = "edited"     // sent a changed reply
	DecisionInstructed = "instructed" // gave instructions instead
	DecisionDismissed  = "dismissed"  // declined a rule card; on older items: dismissed without a reason

	// Why the owner dismissed an item. Only noise means "should not have come to me".
	DecisionNoise            = "noise"             // should not have been raised at all
	DecisionHandledElsewhere = "handled_elsewhere" // needed handling; the owner did it in Slack / the ticket himself
	DecisionStale            = "stale"             // no longer relevant (or no reason given)
)

// SetDecision records how the owner settled an item.
func (b *Box) SetDecision(id, decision string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		it.Decision = decision
		return it, nil
	})
}

// LatestByRef returns the newest item with this ref, finished or not.
func (b *Box) LatestByRef(ref string) (Item, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items, err := b.load()
	if err != nil {
		return Item{}, false, err
	}
	var best Item
	found := false
	for _, it := range items {
		if it.Ref == ref && (!found || it.Updated.After(best.Updated)) {
			best, found = it, true
		}
	}
	return best, found, nil
}

// SetSlackTS remembers where the item was posted in Slack.
func (b *Box) SetSlackTS(id, ts string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		it.SlackTS = ts
		return it, nil
	})
}

// BySlackTS returns the item whose Slack copy has this ts.
func (b *Box) BySlackTS(ts string) (Item, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items, err := b.load()
	if err != nil {
		return Item{}, false, err
	}
	for _, it := range items {
		if it.SlackTS != "" && it.SlackTS == ts {
			return it, true, nil
		}
	}
	return Item{}, false, nil
}

// Get returns one item.
func (b *Box) Get(id string) (Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.get(id)
}

// List returns items, newest first; with all=false only unfinished ones.
func (b *Box) List(all bool) ([]Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items, err := b.load()
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if all || it.Status != StatusDone {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Answer records the owner's decision on an open item.
func (b *Box) Answer(id, answer string) (Item, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return Item{}, errors.New("回答是空的")
	}
	return b.update(id, func(it Item) (Item, error) {
		if it.Status != StatusOpen && !(it.Status == StatusDone && it.TaskClosed && !it.Owner) {
			return it, ErrClosed
		}
		it.Status, it.Answer, it.TaskClosed, it.HandoffError, it.Failure = StatusAnswered, answer, false, "", ""
		return it, nil
	})
}

// HandoffFailed records that the round carrying the owner's answer failed, with its error.
func (b *Box) HandoffFailed(id, errText string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		if it.Status != StatusAnswered {
			return it, ErrClosed
		}
		it.HandoffError = strings.TrimSpace(errText)
		return it, nil
	})
}

// Resend clears a failed handoff so the same answer can go to the task again.
func (b *Box) Resend(id string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		if it.Status != StatusAnswered {
			return it, ErrClosed
		}
		it.HandoffError = ""
		return it, nil
	})
}

// Note adds findings to an unfinished item. With reopen, an answered item goes back to the owner
// (the task could not carry the answer out) and the previous answer is cleared.
func (b *Box) Note(id, analysis string, reopen bool) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		if it.Status == StatusDone {
			return it, ErrClosed
		}
		it.Analysis = strings.TrimSpace(strings.TrimSpace(it.Analysis) + "\n" + strings.TrimSpace(analysis))
		if reopen {
			it.Status, it.Answer = StatusOpen, ""
		}
		return it, nil
	})
}

// Failed puts an item back in front of the owner because the action they approved did not go through: why
// is added to the findings under head and kept on the item to show on the card.
func (b *Box) Failed(id, head, why string) (Item, error) {
	why = strings.TrimSpace(why)
	return b.update(id, func(it Item) (Item, error) {
		if it.Status == StatusDone {
			return it, ErrClosed
		}
		it.Analysis = strings.TrimSpace(strings.TrimSpace(it.Analysis) + "\n" + head + "\n" + why)
		it.Status, it.Answer, it.Failure = StatusOpen, "", why
		return it, nil
	})
}

// HandBack is the task returning the owner's answer undone (todo note --reopen). Each time an answered
// item comes back counts; at MaxReopens it becomes the owner's own step.
func (b *Box) HandBack(id, analysis string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		if it.Status == StatusDone {
			return it, ErrClosed
		}
		it.Analysis = strings.TrimSpace(strings.TrimSpace(it.Analysis) + "\n" + strings.TrimSpace(analysis))
		if it.Status == StatusAnswered {
			it.Reopens++
			if it.Reopens >= MaxReopens && !it.Owner {
				it.Owner = true
				it.Analysis += "\n" + fmt.Sprintf(ownerStepNote, it.Reopens)
			}
		}
		it.Status, it.Answer = StatusOpen, ""
		return it, nil
	})
}

// Close finishes an item with a short outcome (what was sent, or why it is closed).
func (b *Box) Close(id, outcome string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		it.Status, it.Outcome = StatusDone, strings.TrimSpace(outcome)
		return it, nil
	})
}

// CloseByTask is Close by the item's own task, not the owner: the owner can still answer it afterwards
// (an answer they were writing when the task closed it is not thrown away).
func (b *Box) CloseByTask(id, outcome string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		it.Status, it.Outcome, it.TaskClosed = StatusDone, strings.TrimSpace(outcome), true
		return it, nil
	})
}

// CloseIfOpen closes an item only while it is still open (ErrClosed otherwise), atomically.
func (b *Box) CloseIfOpen(id, outcome string) (Item, error) {
	return b.update(id, func(it Item) (Item, error) {
		if it.Status != StatusOpen {
			return it, ErrClosed
		}
		it.Status, it.Outcome = StatusDone, strings.TrimSpace(outcome)
		return it, nil
	})
}

func (b *Box) update(id string, fn func(Item) (Item, error)) (Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	it, err := b.get(id)
	if err != nil {
		return Item{}, err
	}
	next, err := fn(it)
	if err != nil {
		return it, err
	}
	next.Updated = b.now()
	return next, b.save(next)
}

func (b *Box) get(id string) (Item, error) {
	if !idPattern.MatchString(id) {
		return Item{}, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(b.dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("read todo %s: %w", id, err)
	}
	var it Item
	if err := json.Unmarshal(data, &it); err != nil {
		return Item{}, fmt.Errorf("decode todo %s: %w", id, err)
	}
	return it, nil
}

func (b *Box) load() ([]Item, error) {
	files, err := filepath.Glob(filepath.Join(b.dir, "td-*.json"))
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(files))
	for _, f := range files {
		it, err := b.get(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			continue // a damaged file must not hide the rest
		}
		items = append(items, it)
	}
	return items, nil
}

func (b *Box) save(it Item) error {
	data, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return fmt.Errorf("encode todo: %w", err)
	}
	tmp := filepath.Join(b.dir, it.ID+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write todo: %w", err)
	}
	return os.Rename(tmp, filepath.Join(b.dir, it.ID+".json"))
}

func newID() (string, error) {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("todo id: %w", err)
	}
	return "td-" + hex.EncodeToString(buf[:]), nil
}
