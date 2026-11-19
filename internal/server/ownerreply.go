package server

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/replay"
	"vigil/internal/slackctl"
)

// Owner replies: what the owner actually wrote in a to-do's conversation, by hand. A to-do the owner
// handled elsewhere, or answered in Slack without touching the to-do, still holds the best lesson there
// is: the draft next to the reply the owner chose to send. Vigil reads those threads (read only) and
// keeps each reply as a sample for the self-evolution task.
const (
	ownerRepliesFile   = "owner_replies.jsonl"
	ownerHarvestEvery  = 30 * time.Minute
	ownerHarvestWindow = 3 * 24 * time.Hour // to-dos younger than this are read; replies count up to this long after the message
	ownerHistoryLimit  = 50                 // recent DM messages read, newest first
	ownerWakeBelow     = 0.6                // a reply this far from the draft wakes the evolution task
	ownerTextLen       = 1200
	ownerSettledRead   = 24 * time.Hour   // a to-do settled longer ago than this is no longer read
	ownerAfterClose    = 30 * time.Minute // replies count up to this long after the to-do was settled; later is a new topic
)

// agentMark ends every message an agent sends with the owner's account (the Slack MCP signature);
// those are the agent's words, not the owner's.
const agentMark = "*发送工具*"

// historyReader lists a channel's recent messages, newest first. DMs need it: the owner answers a DM
// in the conversation, not in a thread under the message.
type historyReader interface {
	History(ctx context.Context, channel string, limit int) ([]slackctl.Message, error)
}

// OwnerReply is one message the owner wrote by hand in the conversation of a to-do.
type OwnerReply struct {
	Todo     string    `json:"todo"`
	Task     string    `json:"task"`
	Kind     string    `json:"kind,omitempty"`
	Title    string    `json:"title"`
	Decision string    `json:"decision,omitempty"` // how the to-do was settled when the reply was seen ("" while open)
	Draft    string    `json:"draft,omitempty"`    // the agent's suggested reply
	Shadow   bool      `json:"shadow,omitempty"`   // Draft is what the agent would have sent, not a reply it offered
	Text     string    `json:"text"`               // what the owner wrote
	Channel  string    `json:"channel"`
	TS       string    `json:"ts"`
	At       time.Time `json:"at"`   // when the owner wrote it
	Seen     time.Time `json:"seen"` // when Vigil found it; signals are listed by this, since harvests lag
}

type ownerReplyLog = recordLog[OwnerReply]

func ownerReplyKey(r OwnerReply) string { return r.Todo + ":" + r.TS }

func (s *Server) ownerReplies() *ownerReplyLog {
	s.ownerOnce.Do(func() {
		s.ownerLog = &ownerReplyLog{path: filepath.Join(s.Store.Root(), ownerRepliesFile), id: ownerReplyKey}
	})
	return s.ownerLog
}

// HarvestLoop collects owner replies and what askers said after agent replies on its own clock, so slow Slack reads never hold up follow-ups.
func (s *Server) HarvestLoop(ctx context.Context) {
	tick := time.NewTicker(ownerHarvestEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.harvestOwnerReplies(ctx)
			s.watchAftermath(ctx)
		}
	}
}

// replyWindow is the stretch of one conversation whose owner messages answer one to-do: from when the
// to-do was raised until the next to-do raised in the same conversation, at most ownerHarvestWindow.
type replyWindow struct {
	item        inbox.Item
	channel, ts string
	from, until float64
	read        bool // false: only bounds its neighbours (closed long ago), not read
	threadOnly  bool // the draft went out as is: DM talk outside its thread is about something else
}

// conversationKey groups to-dos that share messages: a DM is one conversation, a channel has one per thread.
func conversationKey(channel, ts string) string {
	if strings.HasPrefix(channel, "D") {
		return channel
	}
	return channel + ":" + ts
}

// replyWindows lists the Slack to-dos worth reading now, each with its own stretch of the conversation.
func (s *Server) replyWindows(items []inbox.Item) []replyWindow {
	now := s.Now()
	var ws []replyWindow
	for _, it := range items {
		channel, ts, ok := slackTarget(it)
		if !ok || strings.HasPrefix(it.Ref, selfTestRef+":") || now.Sub(it.Created) > ownerHarvestWindow {
			continue
		}
		from := max(slackTime(ts), float64(it.Created.UnixNano())/1e9)
		settled := it.Status == inbox.StatusDone
		until := from + ownerHarvestWindow.Seconds()
		if settled {
			until = min(until, float64(it.Updated.Add(ownerAfterClose).UnixNano())/1e9)
		}
		ws = append(ws, replyWindow{item: it, channel: channel, ts: ts, from: from, until: until,
			read: !(settled && now.Sub(it.Updated) > ownerSettledRead), threadOnly: it.Decision == inbox.DecisionAsIs})
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].from < ws[j].from })
	next := map[string]float64{}
	for i := len(ws) - 1; i >= 0; i-- {
		key := conversationKey(ws[i].channel, ws[i].ts)
		if n, ok := next[key]; ok {
			ws[i].until = min(ws[i].until, n)
		}
		next[key] = ws[i].from
	}
	return ws
}

// harvestOwnerReplies reads the conversations of recent Slack to-dos and records the owner's own replies.
func (s *Server) harvestOwnerReplies(ctx context.Context) {
	if s.Threads == nil || s.Inbox == nil {
		return
	}
	items, err := s.Inbox.List(true)
	if err != nil {
		s.logf("owner replies: %v", err)
		return
	}
	self := ""
	reads := map[string][]slackctl.Message{} // one read per thread and per DM history each pass
	for _, w := range s.replyWindows(items) {
		if !w.read {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if self == "" {
			if self, err = s.Threads.SelfID(ctx); err != nil {
				s.logf("owner replies: who am I: %v", err)
				return
			}
		}
		msgs, err := s.conversation(ctx, reads, w.channel, w.ts, !w.threadOnly)
		if err != nil {
			s.logf("owner replies %s: %v", w.item.ID, err)
			continue
		}
		for _, m := range ownMessages(msgs, self, w.from, w.until) {
			s.keepOwnerReply(w.item, w.channel, m)
		}
	}
}

// slackTarget reads where a to-do's conversation lives; system to-dos and decision cards have none.
func slackTarget(it inbox.Item) (channel, ts string, ok bool) {
	if it.TaskID == inbox.SystemTask || isDecisionCard(it) {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(it.Target, "slack:")
	if !ok {
		return "", "", false
	}
	channel, ts, ok = strings.Cut(rest, ":")
	return channel, ts, ok && channel != "" && slackTime(ts) > 0
}

// conversation is the thread under the message, plus, in a DM when withHistory, the recent messages
// around it. Reads are remembered in seen for the rest of the pass.
func (s *Server) conversation(ctx context.Context, seen map[string][]slackctl.Message, channel, ts string, withHistory bool) ([]slackctl.Message, error) {
	threadKey := "thread " + channel + ":" + ts
	msgs, ok := seen[threadKey]
	if !ok {
		var err error
		if msgs, err = s.Threads.Messages(ctx, channel, ts); err != nil {
			return nil, fmt.Errorf("read thread: %w", err)
		}
		seen[threadKey] = msgs
	}
	hr, ok := s.Threads.(historyReader)
	if !ok || !withHistory || !strings.HasPrefix(channel, "D") {
		return msgs, nil
	}
	histKey := "history " + channel
	recent, ok := seen[histKey]
	if !ok {
		var err error
		if recent, err = hr.History(ctx, channel, ownerHistoryLimit); err != nil {
			return nil, fmt.Errorf("read history: %w", err)
		}
		seen[histKey] = recent
	}
	return append(append([]slackctl.Message{}, msgs...), recent...), nil
}

// ownMessages keeps what the owner wrote by hand in (from, until].
func ownMessages(msgs []slackctl.Message, self string, from, until float64) []slackctl.Message {
	seen := map[string]bool{}
	var out []slackctl.Message
	for _, m := range msgs {
		at := slackTime(m.TS)
		text := strings.TrimSpace(m.Text)
		if m.User != self || at <= from || at > until || seen[m.TS] || !byHand(text) {
			continue
		}
		seen[m.TS] = true
		out = append(out, m)
	}
	return out
}

var slackMention = regexp.MustCompile(`<@[A-Z0-9]+>`)

// byHand tells the owner's own words from an agent's send, Vigil's posts (mirror, self-test),
// corrections and a bare mention.
func byHand(text string) bool {
	if strings.TrimSpace(slackMention.ReplaceAllString(text, "")) == "" {
		return false
	}
	for _, p := range []string{mirrorMark, mirrorCard, selfTestMark} {
		if strings.HasPrefix(text, p) {
			return false
		}
	}
	return !strings.Contains(text, agentMark) && !correctionText.MatchString(text)
}

func (s *Server) keepOwnerReply(it inbox.Item, channel string, m slackctl.Message) {
	r := OwnerReply{Todo: it.ID, Task: it.TaskID, Kind: it.Kind, Title: it.Title, Decision: it.Decision,
		Draft: clipRunes(draftOf(it), ownerTextLen), Shadow: strings.TrimSpace(it.Reply) == "" && it.Shadow != "", Text: clipRunes(strings.TrimSpace(m.Text), ownerTextLen),
		Channel: channel, TS: m.TS, At: time.Unix(0, int64(slackTime(m.TS)*1e9)), Seen: s.Now()}
	_, seen, err := s.ownerReplies().addUnless(r, func(OwnerReply) bool { return false })
	if err != nil {
		s.logf("owner reply %s: %v", it.ID, err)
		return
	}
	if seen {
		return
	}
	s.closeAnsweredByHand(it, r.Text)
	if r.Draft == "" {
		return
	}
	if it.Decision != "" && it.Kind != "" {
		s.proposeFor(it.TaskID, it.Kind) // a new hand reply is new evidence about the kind
	}
	if replay.Similarity(r.Draft, r.Text) >= ownerWakeBelow {
		return
	}
	what := "草稿"
	if r.Shadow {
		what = "agent 私下拟的回复"
	}
	s.wakeEvolve("你在「" + it.Title + "」里亲手回了，和" + what + "不一样（待办 " + it.ID + "）")
}

// ownerRepliesSince lists the samples found at or after from, oldest first, at most the newest max.
func ownerRepliesSince(l *ownerReplyLog, from time.Time, max int) ([]OwnerReply, error) {
	all, err := l.list()
	if err != nil {
		return nil, err
	}
	var out []OwnerReply
	for _, r := range all {
		if seen := r.Seen; !seen.IsZero() && !seen.Before(from) || seen.IsZero() && !r.At.Before(from) {
			out = append(out, r)
		}
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out, nil
}

// holdingWords are what the owner says to buy time; a short reply made of them settles nothing.
var holdingWords = []string{"稍等", "等下", "等等", "我看看", "我看下", "看看", "看下", "晚点", "马上", "收到", "好的我", "在看", "我查", "查下", "查一下"}

const holdingMaxRunes = 12

func holdingReply(text string) bool {
	t := strings.TrimSpace(slackMention.ReplaceAllString(text, ""))
	if len([]rune(t)) > holdingMaxRunes {
		return false
	}
	for _, w := range holdingWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// handReplyNoteLen bounds the owner's words quoted in the outcome of a to-do closed for him.
const handReplyNoteLen = 80

// closeAnsweredByHand settles a to-do the owner answered in Slack himself, as "handled elsewhere", so he
// does not have to come back and close it. A to-do only he can do (approve on a page, say) stays open: a
// word in the thread does not mean the work is done. So do rule, revoke and patch cards, settled only
// by an explicit yes or no.
func (s *Server) closeAnsweredByHand(it inbox.Item, text string) {
	if it.Status != inbox.StatusOpen || it.Owner || isRule(it) || isRevoke(it) || isPatch(it) || holdingReply(text) {
		return
	}
	note := "你在 Slack 里亲手回了，自动结案：" + clipRunes(strings.TrimSpace(text), handReplyNoteLen)
	if _, err := s.AnswerTodoReason(it.ID, TodoDismiss, note, inbox.DecisionHandledElsewhere); err != nil {
		s.logf("close %s answered by hand: %v", it.ID, err)
	}
}
