package resident

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"vigil/internal/task"
)

// Prompt is an unknown dialog cut out of the agent's screen: what it is about, the command or text it
// shows, and what it asks. The screen above the dialog (earlier output, wrapped prompt text) is dropped.
type Prompt struct {
	Kind     string   // the dialog's heading, e.g. "Bash command"
	Notes    []string // plain lines inside the dialog: a description, the rule that raised it …
	Command  string   // the boxed command or content it asks about, as shown
	Question string   // e.g. "Do you want to proceed?"
	Options  []string // the numbered answers, without their numbers
	Keys     []string // the number that picks each option
}

const (
	promptFallbackLines = 8   // shown when no dialog shape is recognised
	promptMaxLine       = 300 // runes kept of one screen line
	promptMaxCommand    = 12  // lines kept of a boxed command
)

var (
	optionLine = regexp.MustCompile(`^\s*(?:[❯›>]\s*)?(\d+)\.\s+(.+?)\s*$`)
	boxedLine  = regexp.MustCompile(`^\s*[│┃|]\s?(.*)$`)
)

// ReadPrompt finds the dialog at the bottom of screen. A screen without a numbered list of answers is
// kept as its last few lines.
func ReadPrompt(screen string) Prompt {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	first := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if m := optionLine.FindStringSubmatch(lines[i]); m != nil && m[1] == "1" {
			first = i
			break
		}
	}
	q := prevText(lines, first)
	if first < 0 || q < 0 {
		return Prompt{Notes: []string{clipLine(tail(screen, promptFallbackLines))}}
	}
	p := Prompt{Question: strings.TrimSpace(lines[q])}
	// Options may carry indented descriptions or be split by a rule ("4. Chat about this"): take every
	// numbered line that continues the count.
	for _, l := range lines[first:] {
		m := optionLine.FindStringSubmatch(l)
		if m == nil || m[1] != fmt.Sprint(len(p.Options)+1) {
			continue
		}
		p.Options = append(p.Options, m[2])
		p.Keys = append(p.Keys, m[1])
	}
	top := q - 1
	for top >= 0 && !isRule(lines[top], '─') {
		top--
	}
	if top < 0 {
		top = max(q-promptFallbackLines, -1)
	}
	var cmd []string
	for _, l := range lines[top+1 : q] {
		switch {
		case strings.TrimSpace(l) == "" || isRule(l, 0):
		case boxedLine.MatchString(l):
			cmd = append(cmd, clipLine(strings.TrimRight(boxedLine.FindStringSubmatch(l)[1], " ")))
		case p.Kind == "":
			p.Kind = clipLine(strings.TrimSpace(l))
		default:
			p.Notes = append(p.Notes, clipLine(strings.TrimSpace(l)))
		}
	}
	if len(cmd) > promptMaxCommand {
		cmd = append(cmd[:promptMaxCommand], "…")
	}
	p.Command = strings.Join(cmd, "\n")
	return p
}

// Headline is the dialog in one line: its heading and the first note ("Bash command：回报本轮结果").
func (p Prompt) Headline() string {
	parts := []string{}
	if p.Kind != "" {
		parts = append(parts, p.Kind)
	}
	if len(p.Notes) > 0 {
		parts = append(parts, p.Notes[0])
	}
	return strings.Join(parts, "：")
}

// Body is the dialog without its question line: heading, command and notes.
func (p Prompt) Body() string {
	var b []string
	if h := p.Headline(); h != "" {
		b = append(b, h)
	}
	if p.Command != "" {
		b = append(b, p.Command)
	}
	if len(p.Notes) > 1 {
		b = append(b, p.Notes[1:]...)
	}
	return strings.Join(b, "\n")
}

// lasting marks answers that change settings for later prompts too ("Yes, and don't ask again for …");
// those are left to the agent's own session, where the owner sees exactly what they would allow.
var lasting = regexp.MustCompile(`(?i)don't ask again|always|for this session|permanently`)

// Answers are the options the owner may pick from a card: every one that settles only this prompt.
func (p Prompt) Answers() []string {
	var out []string
	for _, o := range p.Options {
		if !lasting.MatchString(o) {
			out = append(out, o)
		}
	}
	return out
}

// Choose is the key that picks the option the owner named by its number or its text (a trailing
// "(Recommended)" may be left off). Unlike Key it allows every option: it is for the owner's own sessions.
func (p Prompt) Choose(text string) (string, bool) {
	text = strings.TrimSpace(text)
	for i, o := range p.Options {
		if text == p.Keys[i] || text == o || text == strings.TrimSpace(strings.TrimSuffix(o, "(Recommended)")) {
			return p.Keys[i], true
		}
	}
	return "", false
}

// Choices lists the options as "1. …" lines.
func (p Prompt) Choices() string {
	out := make([]string, len(p.Options))
	for i, o := range p.Options {
		out[i] = p.Keys[i] + ". " + o
	}
	return strings.Join(out, "\n")
}

// Key is the key that picks answer in this prompt, if the prompt offers it and it settles only this prompt.
func (p Prompt) Key(answer string) (string, bool) {
	for i, o := range p.Options {
		if o == answer && !lasting.MatchString(o) {
			return p.Keys[i], true
		}
	}
	return "", false
}

// Text is the dialog as plain lines for a card or a notice: its body, then the question and answers.
func (p Prompt) Text() string {
	if p.Question == "" {
		return p.Body()
	}
	q := p.Question
	if len(p.Options) > 0 {
		q += " " + strings.Join(p.Options, " / ")
	}
	if b := p.Body(); b != "" {
		return b + "\n" + q
	}
	return q
}

func prevText(lines []string, i int) int {
	for j := i - 1; j >= 0; j-- {
		if strings.TrimSpace(lines[j]) != "" {
			return j
		}
	}
	return -1
}

// isRule reports whether a line is a horizontal rule; ch limits it to one drawing character (0: any).
func isRule(l string, ch rune) bool {
	s := strings.TrimSpace(l)
	if len([]rune(s)) < 10 {
		return false
	}
	for _, r := range s {
		if ch != 0 && r != ch {
			return false
		}
		if ch == 0 && !strings.ContainsRune("─━═╌╍┄┅-", r) {
			return false
		}
	}
	return true
}

func clipLine(s string) string {
	if r := []rune(s); len(r) > promptMaxLine {
		return string(r[:promptMaxLine]) + "…"
	}
	return s
}

// ErrDialogChanged is returned when the agent's screen no longer shows the dialog that was asked about.
var ErrDialogChanged = errors.New("确认框已经变了或已经处理过，去 herdr 的 vigil 会话里看一眼")

// AnswerDialog picks answer in the dialog the task's agent is stopped on. shown is the dialog's Body as the
// owner saw it; nothing is pressed unless the screen still shows that same dialog with that answer on offer,
// so an answer never lands on a different command that came up since.
func (e Executor) AnswerDialog(ctx context.Context, t task.Task, shown, answer string) error {
	name := task.AgentName(t.ID)
	screen, err := e.H.Read(ctx, name, readLines)
	if err != nil {
		return fmt.Errorf("read agent screen: %w", err)
	}
	p := ReadPrompt(screen)
	key, ok := p.Key(answer)
	if !ok || p.Body() != shown {
		return ErrDialogChanged
	}
	if err := e.H.SendKeys(ctx, name, key); err != nil {
		return fmt.Errorf("press %s: %w", key, err)
	}
	return nil
}
