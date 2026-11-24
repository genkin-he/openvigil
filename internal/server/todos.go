package server

import (
	"errors"
	"net/http"
	"strings"

	"vigil/internal/inbox"
	"vigil/internal/runner"
)

// Answer modes for a to-do.
const (
	TodoSend     = "send"     // send this exact text as the reply
	TodoInstruct = "instruct" // the owner's instructions; the task works out what to do and send
	TodoDismiss  = "dismiss"  // nothing to do any more (handled elsewhere, no longer relevant)
	TodoChoose   = "choose"   // one of the item's fixed choices (with an optional remark in text)
)

type todoAnswer struct {
	Mode   string `json:"mode"`
	Text   string `json:"text"`
	Reason string `json:"reason,omitempty"` // with dismiss: noise, handled_elsewhere or stale (default)
	Choice string `json:"choice,omitempty"` // with choose: the label picked
}

var dismissNote = map[string]string{
	inbox.DecisionNoise:            "不该找我",
	inbox.DecisionHandledElsewhere: "我在别处处理了",
	inbox.DecisionStale:            "已经过时，不用处理",
}

type todoNote struct {
	Analysis string `json:"analysis"`
	Reopen   bool   `json:"reopen"`
}

type todoClose struct {
	Outcome string `json:"outcome"`
}

func (s *Server) todoList(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.Inbox.List(r.URL.Query().Get("all") == "1")
}

func (s *Server) todoAdd(_ http.ResponseWriter, r *http.Request) (any, error) {
	var in inbox.Item
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if _, err := s.Store.Get(in.TaskID); err != nil {
		return nil, badRequest("task %q 不存在，--task 要填负责执行回复的任务", in.TaskID)
	}
	if err := inbox.ValidChoices(in.Choices); err != nil {
		return nil, badRequest("%v", err)
	}
	if strings.TrimSpace(in.Kind) == askKind {
		if err := s.mayAsk(in.Ref); err != nil {
			return nil, err
		}
	}
	if in.RunID == "" && s.Engine != nil {
		in.RunID = liveRunOf(s.Engine.Running(), in.TaskID)
	}
	it, created, err := s.Inbox.Add(in)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	what := "todo.updated"
	if created {
		what = "todo.added"
	}
	s.todoChanged(it, what)
	return map[string]any{"item": it, "created": created}, nil
}

func (s *Server) todoAnswer(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b todoAnswer
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Mode == TodoChoose {
		return s.AnswerChoice(r.PathValue("id"), b.Choice, b.Text)
	}
	return s.AnswerTodoReason(r.PathValue("id"), b.Mode, b.Text, b.Reason)
}

// AnswerChoice settles an item with one of its fixed choices: the task is told exactly which one, what it
// leads to, and any remark the owner added.
func (s *Server) AnswerChoice(id, label, remark string) (inbox.Item, error) {
	it, err := s.Inbox.Get(id)
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	c, ok := it.ChoiceByLabel(label)
	if !ok {
		return inbox.Item{}, badRequest("这条待办没有「%s」这个选项", strings.TrimSpace(label))
	}
	text := "我选了「" + strings.TrimSpace(c.Label) + "」"
	if note := strings.TrimSpace(c.Note); note != "" {
		text += "（" + note + "）"
	}
	if remark = strings.TrimSpace(remark); remark != "" {
		text += "。补充：" + remark
	}
	return s.answerTodo(id, TodoInstruct, text, "", true)
}

// askKind marks a question the self-evolution task puts to the owner about why they handled something
// the way they did. One short question a day at most: asking is cheap for the owner only when it is rare.
const askKind = "问你"

// mayAsk refuses a question when one was already asked today, or the last one is still unanswered.
// A retry of the open question (same ref) is let through: Add merges it into that one.
func (s *Server) mayAsk(ref string) error {
	if ref != "" {
		if _, open, err := s.Inbox.OpenByRef(ref); err != nil || open {
			return err
		}
	}
	items, err := s.Inbox.List(true)
	if err != nil {
		return err
	}
	today := s.Now().Format("2006-01-02")
	for _, it := range items {
		if it.Kind != askKind {
			continue
		}
		if it.Status == inbox.StatusOpen {
			return badRequest("上一个问题（%s）我还没答，先不问新的", it.ID)
		}
		if it.Created.In(s.Now().Location()).Format("2006-01-02") == today {
			return badRequest("今天已经问过一个问题了（%s），留到明天", it.ID)
		}
	}
	return nil
}

// AnswerTodo records the owner's decision and hands it to the task that raised the item.
func (s *Server) AnswerTodo(id, mode, text string) (inbox.Item, error) {
	return s.AnswerTodoReason(id, mode, text, "")
}

// dismissReason checks why an item was dismissed; no reason means it is no longer relevant.
func dismissReason(reason string) (string, error) {
	switch reason {
	case "":
		return inbox.DecisionStale, nil
	case inbox.DecisionNoise, inbox.DecisionHandledElsewhere, inbox.DecisionStale:
		return reason, nil
	}
	return "", badRequest("reason 只能是 noise、handled_elsewhere 或 stale")
}

// AnswerTodoReason is AnswerTodo with the reason for a dismissal, which decides what Vigil learns from it.
func (s *Server) AnswerTodoReason(id, mode, text, reason string) (inbox.Item, error) {
	return s.answerTodo(id, mode, text, reason, false)
}

// answerTodo settles an item; picked marks an answer made by pressing one of the item's own buttons,
// which only chooses among options the agent laid out and so is no feedback on its suggested reply.
func (s *Server) answerTodo(id, mode, text, reason string, picked bool) (inbox.Item, error) {
	it, err := s.Inbox.Get(id)
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	switch {
	case isDialogTodo(it):
		return s.settleDialogTodo(it, mode, text)
	case isRule(it):
		return s.settleRule(it, mode, text)
	case isRevoke(it):
		return s.settleRevoke(it, mode)
	case isPatch(it):
		return s.settlePatch(it, mode)
	}
	if it.Owner && mode == TodoSend {
		return s.ownerDone(it)
	}
	if mode == TodoDismiss {
		decision, err := dismissReason(reason)
		if err != nil {
			return inbox.Item{}, err
		}
		closed, err := s.Inbox.Close(id, firstNonEmpty(strings.TrimSpace(text), dismissNote[decision]))
		if err != nil {
			return inbox.Item{}, todoErr(err)
		}
		if it.TaskID == inbox.SystemTask {
			s.setMute(it.Ref, s.Now())
		}
		s.learn(closed, decision)
		s.todoChanged(closed, "todo.closed")
		return closed, nil
	}
	if it.TaskID == inbox.SystemTask {
		s.retrySystem(it) // a system to-do goes away by itself once the check passes
		return it, nil
	}
	if mode != TodoSend && mode != TodoInstruct {
		return inbox.Item{}, badRequest("mode 只能是 send、instruct 或 dismiss")
	}
	if _, err := s.Store.Get(it.TaskID); err != nil {
		return inbox.Item{}, badRequest("负责这条待办的任务 %s 已经不在了", it.TaskID)
	}
	answered, err := s.Inbox.Answer(id, text)
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	s.Engine.Submit(runner.Request{TaskID: it.TaskID, Trigger: runner.ByAnswer, Input: todoInput(answered, mode, s.vigilBin())})
	s.todoChanged(answered, "todo.answered")
	s.learnFrom(answered, decisionFor(it, mode, text), !picked)
	return answered, nil
}

// ownerDone closes a step only the owner could take, once they say it is done. It does not run the task:
// the agent is the one that could not do it, and handing it back would only ask it to try again.
// The task's next round checks the change took effect.
func (s *Server) ownerDone(it inbox.Item) (inbox.Item, error) {
	closed, err := s.Inbox.Close(it.ID, "我已亲手做完："+clipRunes(oneLine(it.Reply), 200))
	if err != nil {
		return inbox.Item{}, todoErr(err)
	}
	s.todoChanged(closed, "todo.closed")
	return closed, nil
}

func (s *Server) todoNote(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b todoNote
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	note := func(id, analysis string) (inbox.Item, error) { return s.Inbox.Note(id, analysis, false) }
	if b.Reopen {
		note = s.Inbox.HandBack
	}
	it, err := note(r.PathValue("id"), b.Analysis)
	if err != nil {
		return nil, todoErr(err)
	}
	s.todoChanged(it, "todo.noted")
	return it, nil
}

func (s *Server) todoClose(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b todoClose
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	it, err := s.Inbox.CloseByTask(r.PathValue("id"), b.Outcome)
	if err != nil {
		return nil, todoErr(err)
	}
	s.todoChanged(it, "todo.closed")
	return it, nil
}

func (s *Server) todoChanged(it inbox.Item, what string) {
	s.Engine.Hub().Publish(runner.Event{Type: what, TaskID: it.TaskID, Text: it.Title})
	s.mirrorTodo(it, what)
}

func (s *Server) vigilBin() string {
	if s.VigilBin != "" {
		return s.VigilBin
	}
	return "vigil"
}

// todoInput tells the task what the owner decided and how to close the loop.
func todoInput(it inbox.Item, mode, bin string) string {
	var b strings.Builder
	b.WriteString("我在 Vigil 待办 " + it.ID + " 里做了决定，请照办。\n")
	b.WriteString("事项：" + it.Title + "\n")
	for _, kv := range [][2]string{{"背景", it.Detail}, {"链接", it.Link}, {"ref", it.Ref}, {"回复目标", it.Target}, {"之前的分析", it.Analysis}} {
		if strings.TrimSpace(kv[1]) != "" {
			b.WriteString(kv[0] + "：" + kv[1] + "\n")
		}
	}
	if it.Status == inbox.StatusAnswered && strings.TrimSpace(it.Outcome) != "" {
		b.WriteString("这条待办之前被你结了（" + it.Outcome + "），但我还是做了决定，以我这次的决定为准；要撤回或改正之前做过的，一并处理。\n")
	}
	if mode == TodoSend {
		b.WriteString("请把下面这段原文一字不改地发到回复目标（例如 Slack 线程用 slack_send_message 带 thread_ts；别的平台用对应的评论 / 回复工具）：\n" + it.Answer + "\n")
	} else {
		b.WriteString("我的意见：" + it.Answer + "\n按我的意见处理；要回消息的，以我的口吻写，简短，事实以分析为准、不编造，不替我许诺。\n")
	}
	b.WriteString("做完执行：" + bin + " todo done " + it.ID + " --outcome \"<实际发出的原文，或做了什么>\"\n")
	b.WriteString("做不了（没权限、找不到目标、需要我再定）就执行：" + bin + " todo note " + it.ID + " --reopen --analysis \"<卡在哪>\"，它会回到我的待办里。")
	return b.String()
}

func todoErr(err error) error {
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		return &httpError{code: http.StatusNotFound, msg: err.Error()}
	case errors.Is(err, inbox.ErrClosed):
		return &httpError{code: http.StatusConflict, msg: err.Error()}
	default:
		return badRequest("%v", err)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
