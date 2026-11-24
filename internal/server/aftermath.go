package server

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigil/internal/slackctl"
	"vigil/internal/store"
)

// Aftermath: what people said after an agent answered them with the owner's account. Once replies go out
// without a draft for the owner to check, the asker's next words are the main sign of whether the answer
// was right:「还是不行」says it was not, long before the owner notices. Vigil finds the agent's replies
// itself (the Slack messages a round took in, then the signed reply after each), reads what the asker said
// next, and hands pushback to the self-evolution task as soon as it shows up.
const (
	aftermathFile     = "aftermath.jsonl"
	aftermathWindow   = 3 * time.Hour // runs this recent are looked at; an asker's words count this long after the reply
	aftermathMaxReads = 30            // conversations read in one pass
	aftermathTextLen  = 600
	thanksMaxLen      = 12 // longer than this, an「OK，…」goes on to say something else
)

// Verdicts on an agent reply, from what the asker said next.
const (
	VerdictPushback = "pushback" // it did not work, or it was not what they asked
	VerdictThanks   = "thanks"   // settled
	VerdictFollowup = "followup" // they asked something more
)

// Aftermath is one agent reply and what the asker said after it.
type Aftermath struct {
	Task      string    `json:"task"`
	Run       string    `json:"run"`
	Channel   string    `json:"channel"`
	Asker     string    `json:"asker"`
	Asked     string    `json:"asked"` // ts of the message the round took in
	Question  string    `json:"question"`
	ReplyTS   string    `json:"reply_ts"`
	Reply     string    `json:"reply"`
	Followups []string  `json:"followups,omitempty"`
	Verdict   string    `json:"verdict,omitempty"`
	At        time.Time `json:"at"`   // when the reply went out
	Seen      time.Time `json:"seen"` // when Vigil last found something new
}

type aftermathLog = recordLog[Aftermath]

func aftermathKey(a Aftermath) string {
	if a.Channel == "" || a.ReplyTS == "" {
		return ""
	}
	return a.Channel + ":" + a.ReplyTS
}

func (s *Server) aftermathLog() *aftermathLog {
	s.afterOnce.Do(func() {
		s.afterLog = &aftermathLog{path: filepath.Join(s.Store.Root(), aftermathFile), id: aftermathKey}
	})
	return s.afterLog
}

// slackEvent is one Slack message a round took in, as the stream writes it into the run's input.
type slackEvent struct {
	channel, ts, thread, user, text string
}

var slackEventLine = regexp.MustCompile(`SLACK (\S+?):(\d+\.\d+) thread=(\S+) user=(\S+) .*?self=0 \| ?(.*)`)

// slackEvents lists the messages from other people in a run's input.
func slackEvents(input string) []slackEvent {
	var out []slackEvent
	for _, line := range strings.Split(input, "\n") {
		m := slackEventLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ev := slackEvent{channel: m[1], ts: m[2], user: m[4], text: m[5]}
		if m[3] != "-" {
			ev.thread = m[3]
		}
		out = append(out, ev)
	}
	return out
}

// watchAftermath looks at the Slack messages recent rounds took in and records the agent's reply to each,
// with what the asker said after it.
func (s *Server) watchAftermath(ctx context.Context) {
	if s.Threads == nil {
		return
	}
	self, err := s.Threads.SelfID(ctx)
	if err != nil {
		s.logf("aftermath: who am I: %v", err)
		return
	}
	reads := map[string][]slackctl.Message{}
	for _, w := range s.aftermathRuns() {
		for _, ev := range slackEvents(w.Input) {
			if ctx.Err() != nil || len(reads) >= aftermathMaxReads {
				return
			}
			root := ev.thread
			if root == "" {
				root = ev.ts
			}
			msgs, err := s.conversation(ctx, reads, ev.channel, root, ev.thread == "")
			if err != nil {
				s.logf("aftermath %s: %v", ev.channel, err)
				continue
			}
			if a, ok := s.aftermathOf(w, ev, msgs, self); ok {
				s.keepAftermath(a)
			}
		}
	}
}

// aftermathRuns are the main-lane runs that started within aftermathWindow, the self-evolution task's excluded.
func (s *Server) aftermathRuns() []store.RunRecord {
	tasks, _ := s.Store.List()
	cutoff := s.Now().Add(-aftermathWindow)
	var out []store.RunRecord
	for _, t := range tasks {
		if s.Evolve != nil && t.ID == s.Evolve.Task {
			continue
		}
		runs, err := s.Store.Runs(t.ID, 0)
		if err != nil {
			continue
		}
		for _, r := range runs {
			if r.Job == "" && !r.StartedAt.Before(cutoff) && strings.Contains(r.Input, "SLACK ") {
				out = append(out, r)
			}
		}
	}
	return out
}

// aftermathOf finds the agent's first reply after ev in the conversation and the asker's words after it,
// up to the next message from the owner's account (the agent again, or the owner taking over).
func (s *Server) aftermathOf(run store.RunRecord, ev slackEvent, msgs []slackctl.Message, self string) (Aftermath, bool) {
	sorted := append([]slackctl.Message{}, msgs...)
	sort.SliceStable(sorted, func(i, j int) bool { return slackTime(sorted[i].TS) < slackTime(sorted[j].TS) })
	asked := slackTime(ev.ts)
	a := Aftermath{Task: run.TaskID, Run: run.ID, Channel: ev.channel, Asker: ev.user, Asked: ev.ts,
		Question: clipRunes(strings.TrimSpace(ev.text), aftermathTextLen)}
	seen := map[string]bool{}
	for _, m := range sorted {
		at := slackTime(m.TS)
		if at <= asked || seen[m.TS] {
			continue
		}
		seen[m.TS] = true
		if a.ReplyTS == "" {
			if m.User == self && strings.Contains(m.Text, agentMark) {
				a.ReplyTS, a.Reply = m.TS, clipRunes(strings.TrimSpace(m.Text), aftermathTextLen)
				a.At = time.Unix(0, int64(at*1e9))
			}
			continue
		}
		if m.User == self || at > slackTime(a.ReplyTS)+aftermathWindow.Seconds() {
			break
		}
		if m.User == ev.user && strings.TrimSpace(m.Text) != "" {
			a.Followups = append(a.Followups, clipRunes(strings.TrimSpace(m.Text), aftermathTextLen))
		}
	}
	a.Verdict = verdictOf(a.Followups)
	return a, a.ReplyTS != ""
}

// keepAftermath records a reply, or what is new about it, and wakes self-evolution on fresh pushback.
func (s *Server) keepAftermath(a Aftermath) {
	a.Seen = s.Now()
	l := s.aftermathLog()
	prev, found, err := l.addUnless(a, func(Aftermath) bool { return false })
	if err != nil {
		s.logf("aftermath %s: %v", aftermathKey(a), err)
		return
	}
	if !found {
		prev = Aftermath{} // addUnless hands back the new record itself
	} else {
		if len(a.Followups) <= len(prev.Followups) {
			return
		}
		if _, err := l.update(aftermathKey(a), func(Aftermath) (Aftermath, error) { return a, nil }); err != nil {
			s.logf("aftermath %s: %v", aftermathKey(a), err)
			return
		}
	}
	if a.Verdict == VerdictPushback && prev.Verdict != VerdictPushback {
		s.wakeEvolve(a.Task, "代回「"+clipRunes(oneLine(a.Question), 60)+"」后，对方说「"+
			clipRunes(oneLine(strings.Join(a.Followups, " ")), 80)+"」")
	}
}

var (
	pushbackWords = regexp.MustCompile(`不对|不是这|不是那|还是不|还是没|还是有问题|没解决|没生效|不生效|不管用|没用|搞错|错了|不准|不太对|并没有|没有用`)
	thanksWords   = regexp.MustCompile(`(?i)^\s*(好的?|好嘞|好滴|谢谢|感谢|多谢|收到|ok|okay|了解|明白|懂了|可以了|搞定|解决了|行)|^(\s*(:[a-z0-9_+\-]+:|[👍🙏👌🤝🎉✅]))+\s*$`)
)

// verdictOf reads what the asker said after a reply. Pushback anywhere outweighs thanks.
func verdictOf(followups []string) string {
	if len(followups) == 0 {
		return ""
	}
	for _, f := range followups {
		if pushbackWords.MatchString(f) {
			return VerdictPushback
		}
	}
	for _, f := range followups {
		if len([]rune(strings.TrimSpace(f))) <= thanksMaxLen && thanksWords.MatchString(f) {
			return VerdictThanks
		}
	}
	return VerdictFollowup
}

// aftermathSince lists the replies whose asker said something, seen at or after from, at most the newest max.
func aftermathSince(l *aftermathLog, from time.Time, max int) ([]Aftermath, error) {
	all, err := l.list()
	if err != nil {
		return nil, err
	}
	var out []Aftermath
	for _, a := range all {
		if a.Verdict != "" && !a.Seen.Before(from) {
			out = append(out, a)
		}
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out, nil
}
