package server

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vigil/internal/inbox"
)

// TodoMirror posts each new to-do to the owner's self-DM, so it can be answered from the phone: a reply
// in the to-do's thread answers it exactly like the console buttons do.
type TodoMirror struct {
	Task    string // the stream task that watches Slack; its event lines are checked for answers
	Channel string // the owner's self-DM
	Slack   SlackMessenger
}

// SlackMessenger posts and reads single messages as the owner.
type SlackMessenger interface {
	Post(ctx context.Context, channel, text, thread string) (string, error)
	Reply(ctx context.Context, channel, thread, ts string) (string, error)
}

// mirrorMark starts every message Vigil itself posts in the self-DM; the watch must not act on them.
const (
	mirrorMark   = "【Vigil】"
	mirrorCard   = "【Vigil 待办 "
	mirrorCall   = 30 * time.Second
	mirrorTries  = 3
	mirrorRetry  = 2 * time.Second
	maxCardField = 1200
)

var (
	noiseWords     = words("不该找我", "别找我", "不用找我", "不该问我", "别问我")
	elsewhereWords = words("我处理了", "我回了", "在别处处理了", "我在别处处理了", "处理了", "回过了", "我已回")
	doneWords      = words("做完了", "做完", "改好了", "已改", "已做", "搞定", "done")
	sendWords      = words("发", "发送", "发吧", "发出", "发出去", "好", "好的", "可以", "行", "ok", "1", "y", "yes", "send")
	dismissWords   = words("不用管", "不用", "不管", "忽略", "算了", "不回", "已处理", "关掉", "2", "n", "no")
	declineWords   = words("不要", "不行", "不采纳", "别", "否", "拒绝", "不同意")
	recheckWords   = words("复查", "查", "好了", "登好了", "处理好了", "recheck")
	eventLine      = regexp.MustCompile(`^SLACK (\S+):(\S+) thread=(\S+) user=\S+ dm=\d mention=\d self=(\d) \| (.*)$`)
)

func words(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// mirrorTodo reflects a to-do change in Slack: a card for a new item, a closing note in its thread.
func (s *Server) mirrorTodo(it inbox.Item, what string) {
	m := s.Mirror
	if m == nil || strings.HasPrefix(it.Ref, selfTestRef+":") {
		return
	}
	s.background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), mirrorCall*mirrorTries)
		defer cancel()
		var err error
		switch what {
		case "todo.added":
			err = s.postCard(ctx, it)
		case "todo.closed", "todo.noted":
			cur, gerr := s.waitSlackTS(ctx, it.ID) // the card may still be on its way
			if gerr != nil || cur.SlackTS == "" {
				return
			}
			if what == "todo.closed" {
				_, err = m.Slack.Post(ctx, m.Channel, mirrorMark+"已结案："+firstNonEmpty(cur.Outcome, "已处理"), cur.SlackTS)
			} else if cur.Status == inbox.StatusOpen {
				_, err = m.Slack.Post(ctx, m.Channel, mirrorMark+"又回到待办，补充：\n"+clipRunes(cur.Analysis, maxCardField), cur.SlackTS)
			}
		}
		if err != nil {
			s.logf("todo %s: mirror to Slack: %v", it.ID, err)
		}
	})
}

// postCard posts the card for a new item, retrying a few times: the owner counts on seeing it on the phone.
func (s *Server) postCard(ctx context.Context, it inbox.Item) error {
	m := s.Mirror
	var err error
	for i := 0; i < mirrorTries; i++ {
		var ts string
		if ts, err = m.Slack.Post(ctx, m.Channel, todoCard(it), ""); err == nil {
			_, err = s.Inbox.SetSlackTS(it.ID, ts)
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(mirrorRetry * time.Duration(i+1)):
		}
	}
	return err
}

// waitSlackTS rereads an item until its card has been posted (or gives up quietly).
func (s *Server) waitSlackTS(ctx context.Context, id string) (inbox.Item, error) {
	for {
		cur, err := s.Inbox.Get(id)
		if err != nil || cur.SlackTS != "" {
			return cur, err
		}
		select {
		case <-ctx.Done():
			return cur, nil
		case <-time.After(mirrorRetry):
		}
	}
}

func todoCard(it inbox.Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s】%s\n", mirrorCard, it.ID, it.Title)
	if it.Detail != "" {
		b.WriteString(clipRunes(it.Detail, maxCardField) + "\n")
	}
	if it.Analysis != "" {
		b.WriteString("\n*分析*\n" + clipRunes(it.Analysis, maxCardField) + "\n")
	}
	if it.Reply != "" {
		label := "建议回复"
		switch {
		case it.Owner:
			label = "需要你亲手做"
		case isRule(it):
			label = "规则"
		case isRevoke(it):
			label = "要收回的记忆"
		case isPatch(it):
			label = "代码改动（diff）"
		}
		b.WriteString("\n*" + label + "*\n> " + strings.ReplaceAll(clipRunes(it.Reply, maxCardField), "\n", "\n> ") + "\n")
	}
	if len(it.Choices) > 0 {
		b.WriteString("\n*选项*（回编号或选项原文）\n")
		for i, c := range it.Choices {
			fmt.Fprintf(&b, "%d. %s", i+1, c.Label)
			if c.Note != "" {
				b.WriteString(" — " + c.Note)
			}
			b.WriteString("\n")
		}
	}
	if it.Link != "" {
		b.WriteString("\n原消息：" + it.Link + "\n")
	}
	b.WriteString("\n在这条消息的线程里回复：")
	switch {
	case it.TaskID == inbox.SystemTask:
		b.WriteString("「复查」处理好后重新检查 ·「不用管」24 小时内不再提醒")
	case isRule(it):
		b.WriteString("「发」采纳这条规则 ·「不用管」不采纳 · 写一段新措辞 = 按你的措辞采纳")
	case isRevoke(it):
		b.WriteString("「发」从记忆里收回这一行 ·「不用管」保留")
	case isPatch(it):
		b.WriteString("到控制台看完整 diff 后点「应用」（只能在控制台确认）·「不用管」不采纳")
	case it.Kind == askKind:
		b.WriteString("回「是」「不是」，或直接说原因 ·「不用管」跳过这个问题")
	case it.Owner:
		b.WriteString("「做完了」结案，下一轮会核对 ·「不用管」不做 · 其他文字 = 按你的意思办")
	case it.Reply != "":
		b.WriteString("「发」照建议回复发出 ·「不该找我」·「我处理了」·「不用管」= 过时 · 其他文字 = 按你的意思办")
	default:
		b.WriteString("「不该找我」·「我处理了」·「不用管」= 过时 · 其他文字 = 按你的意思办")
	}
	return b.String()
}

// InterceptSlack takes the Slack watch's event lines that belong to the to-do mirror: Vigil's own posts
// in the self-DM (dropped) and the owner's replies in a to-do thread (answered here, never reaching the
// agent as a new instruction). It reports whether the line was taken.
func (s *Server) InterceptSlack(taskID, line string) bool {
	m := s.Mirror
	if m == nil || taskID != m.Task {
		return false
	}
	g := eventLine.FindStringSubmatch(line)
	if g == nil {
		return false
	}
	s.noteCorrection(g)
	if g[1] != m.Channel || g[4] != "1" {
		return false
	}
	ts, thread, text := g[2], g[3], stripTags(g[5])
	if strings.HasPrefix(text, mirrorMark) || strings.HasPrefix(text, mirrorCard) {
		return true
	}
	if thread == "-" {
		return false
	}
	it, ok, err := s.Inbox.BySlackTS(thread)
	if err != nil || !ok || it.Status != inbox.StatusOpen {
		return false // a finished item's thread is an ordinary conversation again: the watch handles it
	}
	s.background(func() { s.answerFromSlack(it, ts, text) })
	return true
}

func stripTags(snippet string) string {
	for _, tag := range []string{"[补拉] ", "[编辑] "} {
		snippet = strings.TrimPrefix(snippet, tag)
	}
	return strings.TrimSpace(snippet)
}

func (s *Server) answerFromSlack(it inbox.Item, ts, snippet string) {
	m := s.Mirror
	ctx, cancel := context.WithTimeout(context.Background(), mirrorCall)
	defer cancel()
	text := snippet
	if full, err := m.Slack.Reply(ctx, m.Channel, it.SlackTS, ts); err == nil && strings.TrimSpace(full) != "" &&
		!strings.HasPrefix(full, mirrorCard) {
		text = strings.TrimSpace(full) // the event line carries a shortened text
	}
	reply := s.applySlackAnswer(it, text)
	if reply == "" {
		return // the closing note says it
	}
	if _, err := m.Slack.Post(ctx, m.Channel, mirrorMark+reply, it.SlackTS); err != nil {
		s.logf("todo %s: ack in Slack: %v", it.ID, err)
	}
}

// applySlackAnswer turns a reply in a to-do thread into an answer and returns the acknowledgement.
func (s *Server) applySlackAnswer(it inbox.Item, text string) string {
	key := strings.ToLower(strings.Trim(text, " 。.!！~～"))
	if it.Status != inbox.StatusOpen {
		return fmt.Sprintf("这条待办已经是「%s」状态，没再处理。要补充就在控制台里操作。", it.Status)
	}
	if c, ok := pickedChoice(it, key); ok {
		if _, err := s.AnswerChoice(it.ID, c.Label, ""); err != nil {
			return "没处理成：" + err.Error()
		}
		return "收到，选了「" + c.Label + "」，交给 " + it.TaskID + " 照办，办完会在这里说一声。"
	}
	mode, answer, reason := TodoInstruct, text, ""
	switch {
	case noiseWords[key] && !isDecisionCard(it):
		mode, answer, reason = TodoDismiss, "", inbox.DecisionNoise
	case elsewhereWords[key] && !isDecisionCard(it):
		mode, answer, reason = TodoDismiss, "", inbox.DecisionHandledElsewhere
	case dismissWords[key]:
		mode, answer = TodoDismiss, "我在 Slack 里标记为不用管"
	case isPatch(it) && declineWords[key]:
		mode, answer = TodoDismiss, ""
	case isPatch(it):
		// Agent sends show up as the owner here, so a change to Vigil's own code is approved in the console only.
		return "改 Vigil 代码的卡只能在控制台看完 diff 后点「应用」；不要的话回「不用管」。"
	case it.TaskID == inbox.SystemTask:
		mode = TodoInstruct // any other reply means "check again"
	case it.Owner && (sendWords[key] || doneWords[key]):
		mode, answer = TodoSend, it.Reply
	case sendWords[key] && it.Reply != "":
		mode, answer = TodoSend, it.Reply
	case isDecisionCard(it) && declineWords[key]:
		mode, answer = TodoDismiss, ""
	case isDecisionCard(it):
		// Anything but a clear yes or no is not taken as an answer: a stray「为什么？」must not change memory.
		return "这张卡只认「发」（同意）或「不用管」（不同意）。想改措辞，请在控制台里改好再点。"
	case recheckWords[key] && it.Reply == "":
		return "这条没有建议回复，直接写你想怎么办，或回「不用管」结案。"
	}
	if _, err := s.AnswerTodoReason(it.ID, mode, answer, reason); err != nil {
		return "没处理成：" + err.Error()
	}
	switch {
	case mode == TodoDismiss, it.Owner && mode == TodoSend:
		return "" // closing the item posts its outcome in the thread
	case it.TaskID == inbox.SystemTask:
		return "开始复查，通过后这条会自己结案。"
	case isDecisionCard(it):
		return "" // the closing note says what was written or removed
	case mode == TodoSend:
		return "收到，交给 " + it.TaskID + " 照建议回复发出，发完会在这里说一声。"
	}
	return "收到，交给 " + it.TaskID + " 按你的意思办，办完会在这里说一声。"
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// pickedChoice reads a Slack answer as one of the item's choices: its number (1-based) or its label.
func pickedChoice(it inbox.Item, key string) (inbox.Choice, bool) {
	if len(it.Choices) == 0 || isDecisionCard(it) || it.TaskID == inbox.SystemTask {
		return inbox.Choice{}, false
	}
	if n, err := strconv.Atoi(key); err == nil && n >= 1 && n <= len(it.Choices) {
		return it.Choices[n-1], true
	}
	return it.ChoiceByLabel(key)
}
