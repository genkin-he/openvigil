package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/probe"
	"vigil/internal/runner"
	"vigil/internal/slackctl"
)

// Follow-ups: when an agent asks someone something, files a ticket or hands work to another team on the
// owner's behalf, it records what it is waiting for and by when. When that time comes Vigil gives the
// item back to the task that asked: closed if the answer came, nudged once if not, then handed to the owner.
const (
	followFile      = "follows.jsonl"
	FollowWaiting   = "waiting"
	FollowDone      = "done"
	followRecheck   = 2 * time.Hour // a due item handed to its task is not handed again before this
	followMinDue    = 10 * time.Minute
	followMaxDue    = 30 * 24 * time.Hour
	followEvery     = time.Minute
	followFieldLen  = 600
	followMaxHands  = 3               // hand-offs the task let pass without settling the item before it goes to the owner
	followPeekEvery = 5 * time.Minute // how often channel threads with an open follow-up are read for replies
	followReplyLen  = 500
)

// ThreadReader reads Slack threads as the owner. Replies in channel threads that do not mention the
// owner never reach the event stream, so Vigil reads the threads it waits in itself.
type ThreadReader interface {
	Messages(ctx context.Context, channel, thread string) ([]slackctl.Message, error)
	SelfID(ctx context.Context) (string, error)
}

// FollowProbe looks up whether what a follow-up waits for (a merge request, a ticket) is settled.
type FollowProbe interface {
	Check(ctx context.Context, link, ref string) (probe.Result, error)
}

// errNotDue tells handDue that the item changed (settled, snoozed, handed) since it was listed.
var errNotDue = errors.New("not due any more")

// Follow is one thing Vigil waits for on the owner's behalf.
type Follow struct {
	ID      string    `json:"id"`
	Task    string    `json:"task"`           // the task that asked, and handles it when due
	Title   string    `json:"title"`          // what we wait for, one line
	Who     string    `json:"who,omitempty"`  // whom we wait for
	Ask     string    `json:"ask,omitempty"`  // what was asked or filed, and where
	Link    string    `json:"link,omitempty"` // where to look for the answer
	Ref     string    `json:"ref,omitempty"`  // e.g. slack:<channel>:<thread_ts>, a ticket id
	Due     time.Time `json:"due"`
	Nudges  int       `json:"nudges"`
	Status  string    `json:"status"`
	Outcome string    `json:"outcome,omitempty"`
	Handed  time.Time `json:"handed_at,omitzero"`   // when the due item was last given to its task
	Hands   int       `json:"hands,omitempty"`      // hand-offs since the task last settled or snoozed it
	Seen    string    `json:"seen_reply,omitempty"` // ts of the last thread reply handed to the task
	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`
}

type followStore = recordLog[Follow]

func (s *Server) follows() *followStore {
	s.followOnce.Do(func() {
		s.followLog = &followStore{path: filepath.Join(s.Store.Root(), followFile), id: func(f Follow) string { return f.ID }}
	})
	return s.followLog
}

// parseDue reads a due time: an RFC3339 time or a delay such as 90m, 4h, 2d.
func parseDue(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, checkDue(t.Sub(now))
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(raw, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 || days > 30 {
			return time.Time{}, badRequest("due 写成 90m / 4h / 2d，或 RFC3339 时间")
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(raw); err != nil {
			return time.Time{}, badRequest("due 写成 90m / 4h / 2d，或 RFC3339 时间")
		}
	}
	return now.Add(d), checkDue(d)
}

func checkDue(d time.Duration) error {
	if d < followMinDue || d > followMaxDue {
		return badRequest("到期时间要在 %v 到 30 天之后", followMinDue)
	}
	return nil
}

type followBody struct {
	Task  string `json:"task"`
	Title string `json:"title"`
	Who   string `json:"who"`
	Ask   string `json:"ask"`
	Link  string `json:"link"`
	Ref   string `json:"ref"`
	Due   string `json:"due"`
}

func (s *Server) followAdd(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b followBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.Task) == "" || strings.TrimSpace(b.Title) == "" || strings.TrimSpace(b.Due) == "" {
		return nil, badRequest("task、title、due 都要写")
	}
	if _, err := s.Store.Get(b.Task); err != nil {
		return nil, badRequest("没有任务 %s", b.Task)
	}
	now := s.Now()
	due, err := parseDue(b.Due, now)
	if err != nil {
		return nil, err
	}
	due = s.FollowHours.Next(due) // nobody is chased at night or on a weekend
	f := Follow{ID: "fu-" + randHex(3), Task: b.Task, Title: clipRunes(oneLine(b.Title), 200), Who: clipRunes(b.Who, 100),
		Ask: clipRunes(b.Ask, followFieldLen), Link: clipRunes(b.Link, 500), Ref: clipRunes(b.Ref, 200),
		Due: due, Status: FollowWaiting, Created: now, Updated: now}
	added, existed, err := s.follows().addUnless(f, func(x Follow) bool {
		return f.Ref != "" && x.Ref == f.Ref && x.Status == FollowWaiting // already waiting for this: the first record stands
	})
	if err != nil {
		return nil, err
	}
	if !existed {
		s.changed(f.Task, "follow")
	}
	return added, nil
}

// openFollows lists what Vigil is still waiting for, soonest first.
func (s *Server) openFollows() ([]Follow, error) {
	all, err := s.follows().list()
	if err != nil {
		return nil, err
	}
	out := []Follow{}
	for _, f := range all {
		if f.Status == FollowWaiting {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Due.Before(out[j].Due) })
	return out, nil
}

func (s *Server) followList(_ http.ResponseWriter, r *http.Request) (any, error) {
	if r.URL.Query().Get("all") == "1" {
		all, err := s.follows().list()
		if all == nil {
			all = []Follow{}
		}
		return all, err
	}
	return s.openFollows()
}

type followUpdateBody struct {
	Outcome string `json:"outcome"` // done: what came back
	Due     string `json:"due"`     // snooze: the new due time
	Nudged  bool   `json:"nudged"`  // snooze: a reminder was just sent
}

func (s *Server) followDone(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b followUpdateBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.Outcome) == "" {
		return nil, badRequest("outcome 要写：对方怎么答的，或者为什么不用再等")
	}
	f, err := s.follows().update(r.PathValue("id"), func(f Follow) (Follow, error) {
		if f.Status != FollowWaiting {
			return f, badRequest("%s 已经结了", f.ID)
		}
		f.Status, f.Outcome, f.Updated = FollowDone, clipRunes(b.Outcome, followFieldLen), s.Now()
		return f, nil
	})
	if err == nil {
		s.changed(f.Task, "follow")
	}
	return f, err
}

func (s *Server) followSnooze(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b followUpdateBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	now := s.Now()
	due, err := parseDue(b.Due, now)
	if err != nil {
		return nil, err
	}
	due = s.FollowHours.Next(due) // nobody is chased at night or on a weekend
	f, err := s.follows().update(r.PathValue("id"), func(f Follow) (Follow, error) {
		if f.Status != FollowWaiting {
			return f, badRequest("%s 已经结了", f.ID)
		}
		f.Due, f.Handed, f.Hands, f.Updated = due, time.Time{}, 0, now
		if b.Nudged {
			f.Nudges++
		}
		return f, nil
	})
	if err == nil {
		s.changed(f.Task, "follow")
	}
	return f, err
}

// FollowLoop hands due follow-ups to their tasks until ctx ends.
func (s *Server) FollowLoop(ctx context.Context) {
	tick := time.NewTicker(followEvery)
	defer tick.Stop()
	peek := time.NewTicker(followPeekEvery)
	defer peek.Stop()
	s.peekFollows(ctx) // settled while the daemon was down: close now, not in five minutes
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.handDue()
		case <-peek.C:
			s.peekFollows(ctx)
		}
	}
}

// channelThread reads a slack:<channel>:<thread_ts> ref of a channel thread. DMs are left out: every DM
// message reaches the task as an event already.
func channelThread(ref string) (channel, thread string, ok bool) {
	rest, ok := strings.CutPrefix(ref, "slack:")
	if !ok {
		return "", "", false
	}
	channel, thread, ok = strings.Cut(rest, ":")
	return channel, thread, ok && channel != "" && thread != "" && !strings.HasPrefix(channel, "D")
}

func slackTime(ts string) float64 {
	v, _ := strconv.ParseFloat(ts, 64)
	return v
}

// newReply finds the latest message by someone else since the follow-up was registered and since the
// reply last handed over.
func newReply(msgs []slackctl.Message, self string, f Follow) (slackctl.Message, bool) {
	after := max(float64(f.Created.UnixNano())/1e9, slackTime(f.Seen))
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.User != "" && m.User != self && slackTime(m.TS) > after {
			return m, true
		}
	}
	return slackctl.Message{}, false
}

// peekFollows looks early at what open follow-ups wait for: a follow-up whose merge request or ticket is
// settled closes at once, and one whose channel thread got a reply goes to its task.
func (s *Server) peekFollows(ctx context.Context) {
	open, err := s.openFollows()
	if err != nil {
		s.logf("follow-ups: %v", err)
		return
	}
	self := ""
	for _, f := range open {
		if ctx.Err() != nil {
			return
		}
		channel, thread, ok := channelThread(f.Ref)
		if !ok {
			s.probeFollow(ctx, f)
			continue
		}
		if s.Threads == nil {
			continue
		}
		if self == "" {
			if self, err = s.Threads.SelfID(ctx); err != nil {
				s.logf("follow-ups: who am I: %v", err)
				return
			}
		}
		msgs, err := s.Threads.Messages(ctx, channel, thread)
		if err != nil {
			s.logf("follow-up %s: read thread: %v", f.ID, err)
			continue
		}
		reply, ok := newReply(msgs, self, f)
		if !ok {
			continue
		}
		_, err = s.follows().update(f.ID, func(cur Follow) (Follow, error) {
			if cur.Status != FollowWaiting || slackTime(cur.Seen) >= slackTime(reply.TS) {
				return cur, errNotDue
			}
			cur.Seen, cur.Updated = reply.TS, s.Now()
			return cur, nil
		})
		if err != nil {
			if !errors.Is(err, errNotDue) {
				s.logf("follow-up %s: %v", f.ID, err)
			}
			continue
		}
		s.Engine.Submit(runner.Request{TaskID: f.Task, Trigger: runner.ByFollow, Input: followReplyInput(f, reply, s.vigilBin())})
	}
}

// probeFollow closes a follow-up whose merge request or ticket is settled, and gives its task one run
// to wrap up the original matter.
func (s *Server) probeFollow(ctx context.Context, f Follow) {
	if s.Probe == nil {
		return
	}
	res, err := s.Probe.Check(ctx, f.Link, f.Ref)
	if err != nil {
		s.logf("follow-up %s: look up: %v", f.ID, err)
		return
	}
	if !res.Settled {
		return
	}
	closed, err := s.follows().update(f.ID, func(cur Follow) (Follow, error) {
		if cur.Status != FollowWaiting {
			return cur, errNotDue
		}
		cur.Status, cur.Outcome, cur.Updated = FollowDone, "自动查到："+res.Outcome, s.Now()
		return cur, nil
	})
	if err != nil {
		if !errors.Is(err, errNotDue) {
			s.logf("follow-up %s: %v", f.ID, err)
		}
		return
	}
	s.changed(closed.Task, "follow")
	s.Engine.Submit(runner.Request{TaskID: f.Task, Trigger: runner.ByFollow, Input: followSettledInput(closed, res.Outcome)})
}

func followSettledInput(f Follow, outcome string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "跟进 %s 已自动结掉：%s —— %s\n", f.ID, f.Title, outcome)
	if f.Who != "" {
		fmt.Fprintf(&b, "当时在等：%s\n", f.Who)
	}
	if f.Ask != "" {
		fmt.Fprintf(&b, "当时问了 / 提了：%s\n", f.Ask)
	}
	if f.Link != "" {
		fmt.Fprintf(&b, "链接：%s\n", f.Link)
	}
	b.WriteString("\n这条跟进已经关了，不用再操作它。只看原来那件事还要不要收尾（比如告诉提问的人已经办完）：\n")
	b.WriteString("- 要收尾：先读原线程，确认我或别人还没说过，再做；\n")
	b.WriteString("- 不用收尾，或者已经有人说过了：什么都不做。\n")
	return b.String()
}

func followReplyInput(f Follow, reply slackctl.Message, bin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "跟进 %s 的线程里有新回复：%s\n", f.ID, f.Title)
	if f.Who != "" {
		fmt.Fprintf(&b, "在等：%s\n", f.Who)
	}
	if f.Ask != "" {
		fmt.Fprintf(&b, "当时问了 / 提了：%s\n", f.Ask)
	}
	fmt.Fprintf(&b, "新回复（<@%s>，ts %s）：%s\n", reply.User, reply.TS, clipRunes(oneLine(reply.Text), followReplyLen))
	if f.Link != "" {
		fmt.Fprintf(&b, "去这里看：%s\n", f.Link)
	}
	fmt.Fprintf(&b, "ref：%s\n\n", f.Ref)
	b.WriteString("先读整个线程，看这条回复是不是在答我们等的事：\n")
	fmt.Fprintf(&b, "- 答了 / 办完了：照原来的事接着处理（该回的回、该关的关），然后 %s follow done %s --outcome \"<对方怎么答的>\"\n", bin, f.ID)
	b.WriteString("- 还没办完（比如只说在看、稍等）或者跟这件事无关：什么都不用做，继续等，不要回复。\n")
	return b.String()
}

// handDue gives each due follow-up to the task that asked (once per followRecheck).
func (s *Server) handDue() {
	now := s.Now()
	if !s.FollowHours.Contains(now) {
		return // off hours: what is due waits for the next working morning
	}
	all, err := s.follows().list()
	if err != nil {
		s.logf("follow-ups: %v", err)
		return
	}
	due := func(f Follow) bool {
		return f.Status == FollowWaiting && !f.Due.After(now) && (f.Handed.IsZero() || now.Sub(f.Handed) >= followRecheck)
	}
	for _, f := range all {
		if !due(f) {
			continue
		}
		handed, err := s.follows().update(f.ID, func(cur Follow) (Follow, error) {
			if !due(cur) {
				return cur, errNotDue // settled or snoozed meanwhile
			}
			if cur.Hands >= followMaxHands {
				cur.Status, cur.Outcome, cur.Updated = FollowDone, "任务连续几次没处理这条跟进，已转给你", now
				return cur, nil
			}
			cur.Handed, cur.Hands = now, cur.Hands+1
			return cur, nil
		})
		switch {
		case errors.Is(err, errNotDue):
			continue
		case err != nil:
			s.logf("follow-up %s: %v", f.ID, err)
			continue
		case handed.Status == FollowDone:
			s.escalateFollow(handed)
			continue
		}
		s.Engine.Submit(runner.Request{TaskID: f.Task, Trigger: runner.ByFollow, Input: followInput(handed, s.vigilBin())})
	}
}

// escalateFollow hands a follow-up its task kept failing to settle to the owner as a to-do.
func (s *Server) escalateFollow(f Follow) {
	it, created, err := s.Inbox.Add(inbox.Item{TaskID: f.Task, Kind: "跟进无回音", Ref: "follow:" + f.ID, Link: f.Link,
		Title:    "跟进卡住：" + f.Title,
		Detail:   fmt.Sprintf("在等 %s；登记于 %s。到期后交回 %s %d 次，它都没处理（可能那几轮失败了）。", firstNonEmpty(f.Who, "对方"), f.Created.Format("01-02 15:04"), f.Task, followMaxHands),
		Analysis: f.Ask})
	if err != nil {
		s.logf("follow-up %s: escalate: %v", f.ID, err)
		return
	}
	if created {
		s.todoChanged(it, "todo.added")
	}
}

func followInput(f Follow, bin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "跟进到期 %s：%s\n", f.ID, f.Title)
	if f.Who != "" {
		fmt.Fprintf(&b, "在等：%s\n", f.Who)
	}
	if f.Ask != "" {
		fmt.Fprintf(&b, "当时问了 / 提了：%s\n", f.Ask)
	}
	if f.Link != "" {
		fmt.Fprintf(&b, "去这里看：%s\n", f.Link)
	}
	if f.Ref != "" {
		fmt.Fprintf(&b, "ref：%s\n", f.Ref)
	}
	fmt.Fprintf(&b, "登记于 %s，已追问 %d 次。\n\n", f.Created.Format("01-02 15:04"), f.Nudges)
	b.WriteString("先去看对方有没有回、事情有没有办完：\n")
	fmt.Fprintf(&b, "- 回了 / 办完了：照原来的事接着处理（该回复的回复、该关的关），然后 %s follow done %s --outcome \"<对方怎么答的>\"\n", bin, f.ID)
	if f.Nudges == 0 {
		fmt.Fprintf(&b, "- 还没回：以我的口吻在原处礼貌追问一次（只问进展，不催促、不许诺），然后 %s follow snooze %s --due 1d --nudged\n", bin, f.ID)
	} else {
		fmt.Fprintf(&b, "- 还没回：已经追问过，不要再追。写一条待办交给我（--kind 跟进无回音，说明问了什么、等了多久、建议怎么办），然后 %s follow done %s --outcome \"无回音，已转待办 td-…\"\n", bin, f.ID)
	}
	fmt.Fprintf(&b, "- 不用再等了（问题已经不存在、别人已经答了）：%s follow done %s --outcome \"<原因>\"\n", bin, f.ID)
	return b.String()
}
