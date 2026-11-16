package server

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/result"
	"vigil/internal/runner"
	"vigil/internal/schedule"
	"vigil/internal/store"
	"vigil/internal/task"
)

const (
	dialWindow  = 24 * time.Hour
	detailRuns  = 60
	overviewMax = 400
)

// TaskView is a task with its live state, for lists.
type TaskView struct {
	Task      task.Task         `json:"task"`
	State     store.State       `json:"state"`
	NextRun   *time.Time        `json:"next_run,omitempty"`
	Live      *runner.LiveRun   `json:"live,omitempty"`
	Jobs      []runner.LiveRun  `json:"jobs,omitempty"` // background jobs in progress
	LastRun   *store.RunRecord  `json:"last_run,omitempty"`
	Recent    []store.RunRecord `json:"recent"` // last 24h, newest first, for the dial and pulse
	Proposals int               `json:"proposals"`
	NeedsYou  *store.RunRecord  `json:"needs_you,omitempty"`
	Answered  *Answered         `json:"answered,omitempty"` // latest question already answered, run pending
}

// Answered is a question the owner has answered while the follow-up run has not reported yet.
type Answered struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

func (s *Server) view(t task.Task, live map[string]runner.LiveRun, now time.Time) TaskView {
	v := TaskView{Task: t, Recent: []store.RunRecord{}}
	v.State, _ = s.Store.State(t.ID)
	if t.Enabled {
		if next, ok := schedule.Next(t, now); ok {
			v.NextRun = &next
		}
	}
	if lr, ok := live[t.ID]; ok {
		v.Live = &lr
	}
	v.Jobs = s.Engine.Jobs(t.ID)
	runs, _ := s.Store.Runs(t.ID, overviewMax)
	for i := range runs {
		r := runs[i]
		if v.LastRun == nil && r.Status != store.StatusRunning && r.Status != store.StatusSkipped {
			v.LastRun = &runs[i]
		}
		if now.Sub(r.StartedAt) <= dialWindow {
			v.Recent = append(v.Recent, r)
		}
	}
	if v.LastRun != nil && v.LastRun.Status == store.StatusNeedsYou {
		if v.State.AnsweredRun == v.LastRun.ID {
			v.Answered = &Answered{Question: v.LastRun.Question, Answer: v.State.AnsweredText}
		} else {
			v.NeedsYou = v.LastRun
		}
	}
	ps, _ := s.Store.Proposals(t.ID)
	v.Proposals = len(ps)
	return v
}

func (s *Server) overview(_ http.ResponseWriter, _ *http.Request) (any, error) {
	tasks, errs := s.Store.List()
	live := s.Engine.Live()
	now := s.Now()
	views := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, s.view(t, live, now))
	}
	problems := make([]string, 0, len(errs))
	for _, err := range errs {
		problems = append(problems, err.Error())
	}
	todos := []inbox.Item{}
	if s.Inbox != nil {
		open, err := s.Inbox.List(false)
		if err != nil {
			problems = append(problems, "读待办失败："+err.Error())
		} else {
			todos = open
		}
	}
	follows, err := s.openFollows()
	if err != nil {
		problems = append(problems, "读跟进失败："+err.Error())
	}
	return map[string]any{"now": now, "tasks": views, "todos": todos, "follows": follows, "problems": problems, "draining": s.Engine.Draining()}, nil
}

func (s *Server) taskDetail(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	t, err := s.Store.Get(id)
	if err != nil {
		return nil, err
	}
	v := s.view(t, s.Engine.Live(), s.Now())
	runs, err := s.Store.Runs(id, detailRuns)
	if err != nil {
		return nil, err
	}
	if runs == nil {
		runs = []store.RunRecord{}
	}
	if focus := r.URL.Query().Get("run"); focus != "" {
		runs, err = withRun(s.Store, id, focus, runs)
		if err != nil {
			return nil, err
		}
	}
	docs := map[string]string{}
	for _, d := range []string{store.DocBrief, store.DocRunbook, store.DocMemory} {
		docs[strings.TrimSuffix(d, ".md")], _ = s.Store.ReadDoc(id, d)
	}
	yaml, _ := task.Marshal(t)
	proposals, _ := s.Store.Proposals(id)
	if proposals == nil {
		proposals = []store.Proposal{}
	}
	held := s.Forge.H != nil && s.Forge.Held(r.Context(), id)
	return map[string]any{"view": v, "runs": runs, "docs": docs, "yaml": string(yaml), "proposals": proposals, "held": held}, nil
}

type applyBody struct {
	Forge   string  `json:"forge"` // forge session that produced this task (shows forge progress)
	YAML    string  `json:"yaml"`
	Brief   *string `json:"brief"`
	Runbook *string `json:"runbook"`
}

func (s *Server) applyTask(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b applyBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	t, err := task.Parse([]byte(b.YAML))
	if err != nil {
		return nil, badRequest("%v", err)
	}
	_, getErr := s.Store.Get(t.ID)
	created := getErr != nil
	if err := s.Store.Put(t); err != nil {
		return nil, badRequest("%v", err)
	}
	if b.Brief != nil {
		if err := s.Store.WriteDoc(t.ID, store.DocBrief, *b.Brief); err != nil {
			return nil, err
		}
	}
	if b.Runbook != nil {
		if err := s.Store.WriteDoc(t.ID, store.DocRunbook, *b.Runbook); err != nil {
			return nil, err
		}
	}
	if b.Forge != "" {
		if err := s.linkForge(b.Forge, t.ID); err != nil {
			return nil, badRequest("%v", err)
		}
	}
	s.changed(t.ID, "applied")
	return map[string]any{"id": t.ID, "created": created}, nil
}

func (s *Server) changed(id, what string) {
	s.Engine.Hub().Publish(runner.Event{Type: "task.changed", TaskID: id, Text: what})
}

func (s *Server) removeTask(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	if s.Engine.Active(id) {
		return nil, &httpError{code: http.StatusConflict, msg: "任务正在运行（或有后台子任务没跑完），等结束再删除"}
	}
	if err := s.Store.Remove(id); err != nil {
		return nil, err
	}
	s.changed(id, "removed")
	return map[string]string{"id": id}, nil
}

func (s *Server) setEnabled(r *http.Request, on bool) (any, error) {
	id := r.PathValue("id")
	t, err := s.Store.Get(id)
	if err != nil {
		return nil, err
	}
	t.Enabled = on
	if err := s.Store.Put(t); err != nil {
		return nil, err
	}
	if on {
		// Start the timetable from now so enabling never fires a burst of old runs.
		_, _ = s.Store.UpdateState(id, func(st store.State) store.State {
			st.LastScheduledAt = time.Time{}
			return st
		})
		if s.Healer != nil {
			s.Healer.Reset(id)
		}
	}
	s.changed(id, map[bool]string{true: "resumed", false: "paused"}[on])
	return map[string]any{"id": id, "enabled": on}, nil
}

func (s *Server) pauseTask(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.setEnabled(r, false)
}

func (s *Server) resumeTask(_ http.ResponseWriter, r *http.Request) (any, error) {
	return s.setEnabled(r, true)
}

type runBody struct {
	Input string `json:"input"`
	Trial bool   `json:"trial"`
}

func (s *Server) runTask(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	if _, err := s.Store.Get(id); err != nil {
		return nil, err
	}
	var b runBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	trigger := runner.ByManual
	if b.Trial {
		trigger = runner.ByTrial
	}
	s.Engine.Submit(runner.Request{TaskID: id, Trigger: trigger, Input: b.Input})
	return map[string]string{"id": id, "queued_at": s.Now().Format(time.RFC3339Nano)}, nil
}

type textBody struct {
	Text string `json:"text"`
}

func (s *Server) answerTask(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b textBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return s.Answer(r.PathValue("id"), b.Text)
}

// ErrAlreadyAnswered is returned when the latest question of a task already got an answer.
var ErrAlreadyAnswered = &httpError{code: http.StatusConflict, msg: "这个问题已经回答过了，任务正在按那次的回答处理"}

// Answer feeds the owner's answer to a task's latest question into its next run. Used by the API and
// by `vigil answer` and the console. Each question takes one answer; repeats get ErrAlreadyAnswered.
func (s *Server) Answer(id, text string) (map[string]any, error) {
	answer := strings.TrimSpace(text)
	if answer == "" {
		return nil, badRequest("answer is empty")
	}
	if _, err := s.Store.Get(id); err != nil {
		return nil, err
	}
	runs, err := s.Store.Runs(id, 10)
	if err != nil {
		return nil, err
	}
	var asked store.RunRecord
	for _, run := range runs {
		if run.Status == store.StatusNeedsYou {
			asked = run
			break
		}
	}
	if asked.ID != "" {
		if st, _ := s.Store.State(id); st.AnsweredRun == asked.ID {
			return nil, ErrAlreadyAnswered
		}
	}
	if asked.Dialog {
		return s.answerDialog(id, asked, answer)
	}
	req := runner.Request{TaskID: id, Trigger: runner.ByAnswer, Input: "我对上一轮问题的回答：" + answer}
	if asked.Question != "" {
		req.Input = "上一轮你问我：" + asked.Question + "\n" + req.Input
	}
	if len(asked.Denied) > 0 {
		switch AllowDecision(answer) {
		case AllowOnce:
			req.AllowTools = asked.Denied
			req.Input += "\n这些之前被拦的操作这一轮已经放行：" + strings.Join(asked.Denied, "、")
		case AllowAlways:
			if err := s.allowPermanently(id, asked.Denied); err != nil {
				return nil, err
			}
			req.Input += "\n这些之前被拦的操作已经永久放行：" + strings.Join(asked.Denied, "、")
		case AllowDeny:
		default:
			return nil, badRequest("没看懂这个回答是允许一次、永久放行还是不允许，请回复 A（允许一次）、B（永久放行）或 C（不允许）")
		}
	}
	if asked.ID != "" {
		if _, err := s.Store.UpdateState(id, func(st store.State) store.State { st.AnsweredRun, st.AnsweredText = asked.ID, answer; return st }); err != nil {
			return nil, err
		}
		// Remember the decision so the same question is not asked again.
		note := "决策：" + oneLine(asked.Question) + " → " + oneLine(answer)
		if err := s.Store.AppendMemory(id, []string{note}, s.Now()); err != nil {
			return nil, err
		}
	}
	s.Engine.Submit(req)
	s.changed(id, "answered")
	return map[string]any{"id": id, "allowed": req.AllowTools}, nil
}

// Answers to a "these operations were refused" question.
const (
	AllowUnknown = iota
	AllowOnce
	AllowAlways
	AllowDeny
)

// AllowDecision reads an answer to the refused-operations question: by option letter first
// (A once, B always, C deny), then by meaning. Unclear answers are AllowUnknown, never a silent "no".
func AllowDecision(answer string) int {
	a := strings.TrimSpace(answer)
	upper := strings.ToUpper(a)
	switch {
	case strings.HasPrefix(upper, "A"):
		return AllowOnce
	case strings.HasPrefix(upper, "B"):
		return AllowAlways
	case strings.HasPrefix(upper, "C"):
		return AllowDeny
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(a, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("不允许", "不同意", "不行", "不要", "拒绝", "别放", "不放行"):
		return AllowDeny
	case has("永久", "一直", "以后都", "始终", "总是", "长期", "固定放行"):
		return AllowAlways
	case has("允许", "放行", "同意", "可以", "行", "好", "ok", "OK", "yes"):
		return AllowOnce
	}
	return AllowUnknown
}

// allowPermanently adds rules to the task's agent.allowed_tools.
func (s *Server) allowPermanently(id string, rules []string) error {
	t, err := s.Store.Get(id)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if !slices.Contains(t.Agent.AllowedTools, r) {
			t.Agent.AllowedTools = append(t.Agent.AllowedTools, r)
		}
	}
	if err := s.Store.Put(t); err != nil {
		return err
	}
	s.changed(id, "allowed_tools")
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (s *Server) openTask(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	t, err := s.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if t.Mode == task.ModeResident {
		if err := s.Forge.OpenResident(r.Context(), t); err != nil {
			return nil, badRequest("%v", err)
		}
		return map[string]string{"hint": s.raise(r, "已在 herdr 打开「Vigil 常驻 · "+t.Name+"」，可以直接打字；关掉这个 workspace 只是断开查看，agent 继续值守")}, nil
	}
	sessionID := ""
	runs, _ := s.Store.Runs(id, 20)
	for _, run := range runs {
		if run.SessionID != "" {
			sessionID = run.SessionID
			break
		}
	}
	hint := "已在 herdr 打开「Vigil 接管 · " + t.Name + "」，接管期间定时运行会暂停；聊完退出 claude 即可恢复"
	name, err := s.Forge.Open(r.Context(), t, sessionID, func() { hint = s.raise(r, hint) })
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]string{"agent": name, "hint": hint}, nil
}

// raise brings the herdr terminal forward; a failure only changes the hint.
func (s *Server) raise(r *http.Request, hint string) string {
	if s.RaiseTerminal == nil {
		return hint
	}
	if err := s.RaiseTerminal(r.Context()); err != nil {
		s.logf("raise terminal: %v", err)
		return hint + "（没能自动切到终端：" + err.Error() + "）"
	}
	return hint
}

func (s *Server) appendMemory(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	var b textBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if _, err := s.Store.Get(id); err != nil {
		return nil, err
	}
	return map[string]string{"id": id}, s.Store.AppendMemory(id, []string{b.Text}, s.Now())
}

type amendBody struct {
	Content string `json:"content"`
	Reason  string `json:"reason"`
}

func (s *Server) amendRunbook(_ http.ResponseWriter, r *http.Request) (any, error) {
	id := r.PathValue("id")
	t, err := s.Store.Get(id)
	if err != nil {
		return nil, err
	}
	var b amendBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.Content) == "" {
		return nil, badRequest("runbook is empty")
	}
	switch t.Evolve {
	case task.EvolveOff:
		return nil, &httpError{code: http.StatusForbidden, msg: "这个任务不允许 AI 修改 runbook（evolve: off）"}
	case task.EvolveAuto:
		if err := s.Store.WriteDoc(id, store.DocRunbook, b.Content); err != nil {
			return nil, err
		}
		s.changed(id, "runbook")
		return map[string]string{"applied": "true"}, nil
	}
	name, err := s.Store.SaveProposal(id, b.Content, b.Reason, s.Now())
	if err != nil {
		return nil, err
	}
	s.changed(id, "proposal")
	return map[string]string{"proposal": name, "applied": "false"}, nil
}

func (s *Server) putDoc(_ http.ResponseWriter, r *http.Request) (any, error) {
	id, doc := r.PathValue("id"), r.PathValue("doc")+".md"
	if _, err := s.Store.Get(id); err != nil {
		return nil, err
	}
	var b textBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if err := s.Store.WriteDoc(id, doc, b.Text); err != nil {
		return nil, badRequest("%v", err)
	}
	s.changed(id, "doc")
	return map[string]string{"id": id}, nil
}

type resolveBody struct {
	Approve bool `json:"approve"`
}

func (s *Server) resolveProposal(_ http.ResponseWriter, r *http.Request) (any, error) {
	var b resolveBody
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if err := s.Store.ResolveProposal(id, r.PathValue("name"), b.Approve); err != nil {
		return nil, badRequest("%v", err)
	}
	s.changed(id, "proposal")
	return map[string]bool{"approved": b.Approve}, nil
}

func (s *Server) runDone(_ http.ResponseWriter, r *http.Request) (any, error) {
	data, err := readAll(r)
	if err != nil {
		return nil, err
	}
	res, err := result.Parse(data)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	if err := s.Engine.Report(r.PathValue("run"), res); err != nil {
		return nil, err
	}
	return map[string]string{"status": res.Status}, nil
}

// withRun makes sure the run the console points at (a dot clicked on the dial) is in the list,
// even when it is older than the latest detailRuns.
func withRun(st *store.Store, taskID, runID string, runs []store.RunRecord) ([]store.RunRecord, error) {
	for _, r := range runs {
		if r.ID == runID {
			return runs, nil
		}
	}
	all, err := st.Runs(taskID, 0)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.ID == runID {
			return append(runs, r), nil
		}
	}
	return runs, nil
}
