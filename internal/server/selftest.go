package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"vigil/internal/inbox"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// SelfTest drives the paths that only work end to end — a Slack message reaching the watch and its
// reply landing in the thread, a to-do answer being sent, a background job finishing — through the
// real services, in the owner's self-DM.
type SelfTest struct {
	Task    string // stream task that watches Slack (and owns the test to-do and job)
	Channel string // the owner's self-DM
	Slack   SlackClient
	Step    time.Duration // how long each step may take
	Poll    time.Duration
	Every   time.Duration // automatic runs (after an install) are skipped when the last one is younger
	Path    string        // where the latest report is kept across restarts (optional)

	mu   sync.Mutex
	last *SelfTestReport
}

// SlackClient posts and reads messages as the owner.
type SlackClient interface {
	Post(ctx context.Context, channel, text, thread string) (string, error)
	Thread(ctx context.Context, channel, ts string) ([]string, error)
}

// SelfTestReport is one self-test run.
type SelfTestReport struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"` // running | pass | fail
	Started time.Time      `json:"started_at"`
	Ended   time.Time      `json:"ended_at,omitempty"`
	Steps   []SelfTestStep `json:"steps"`
}

// SelfTestStep is one checked path.
type SelfTestStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Took   string `json:"took"`
}

const (
	selfTestRef  = "selftest"
	selfTestMark = "【Vigil 自测"
)

func (s *Server) selfTestStatus(_ http.ResponseWriter, _ *http.Request) (any, error) {
	if s.SelfTest == nil {
		return nil, badRequest("没有配置自测（config.yaml 的 slack.task / slack.self_dm）")
	}
	return s.SelfTest.Last(), nil
}

// selfTestStart starts a run in the background; auto=1 (after an install) skips when one ran recently.
func (s *Server) selfTestStart(_ http.ResponseWriter, r *http.Request) (any, error) {
	st := s.SelfTest
	if st == nil {
		return nil, badRequest("没有配置自测（config.yaml 的 slack.task / slack.self_dm）")
	}
	if _, err := s.Store.Get(st.Task); err != nil {
		return nil, badRequest("自测用的任务 %s 不存在", st.Task)
	}
	rep, _ := s.startSelfTest(r.URL.Query().Get("auto") == "1")
	return rep, nil
}

// startSelfTest begins a run unless one is going (or, for auto, one passed recently).
func (s *Server) startSelfTest(auto bool) (SelfTestReport, bool) {
	rep, started := s.SelfTest.begin(s.Now(), auto)
	if started {
		base := s.Base
		if base == nil {
			base = context.Background()
		}
		go s.runSelfTest(base, rep.ID) // stops with the daemon; a report left running reads as failed on restart
	}
	return rep, started
}

// Last returns the latest report (nil before the first run).
func (st *SelfTest) Last() *SelfTestReport {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last == nil {
		return nil
	}
	cp := *st.last
	cp.Steps = append([]SelfTestStep(nil), st.last.Steps...)
	return &cp
}

// Load restores the latest report a previous daemon saved.
func (st *SelfTest) Load() error {
	if st.Path == "" {
		return nil
	}
	data, err := os.ReadFile(st.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read self-test report: %w", err)
	}
	var rep SelfTestReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return fmt.Errorf("decode self-test report: %w", err)
	}
	if rep.Status == "running" {
		rep.Status = "fail" // cut off by a restart
	}
	st.mu.Lock()
	st.last = &rep
	st.mu.Unlock()
	return nil
}

// save writes the latest report; callers hold st.mu.
func (st *SelfTest) save() {
	if st.Path == "" || st.last == nil {
		return
	}
	if data, err := json.MarshalIndent(st.last, "", "  "); err == nil {
		_ = os.WriteFile(st.Path, data, 0o600)
	}
}

func (st *SelfTest) begin(now time.Time, auto bool) (SelfTestReport, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last != nil && st.last.Status == "running" {
		return *st.last, false
	}
	if auto && st.last != nil && st.last.Status == "pass" && now.Sub(st.last.Started) < st.Every {
		return *st.last, false
	}
	st.last = &SelfTestReport{ID: nonce(), Status: "running", Started: now}
	st.save()
	return *st.last, true
}

func (st *SelfTest) record(step SelfTestStep) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.last.Steps = append(st.last.Steps, step)
}

func (s *Server) runSelfTest(ctx context.Context, id string) {
	st := s.SelfTest
	thread := s.selfTestStep(ctx, "Slack 消息 → 值守在线程里回复", func() (string, error) { return s.stepListen(ctx, id) })
	if thread != "" {
		s.selfTestStep(ctx, "待办「发送这条回复」→ 发到 Slack 并结案", func() (string, error) { return "", s.stepTodo(ctx, id, thread) })
	}
	s.selfTestStep(ctx, "后台子任务跑完并回报", func() (string, error) { return "", s.stepJob(ctx, id) })

	st.mu.Lock()
	st.last.Ended = s.Now()
	st.last.Status = "pass"
	var failed []string
	for _, step := range st.last.Steps {
		if !step.OK {
			st.last.Status = "fail"
			failed = append(failed, step.Name+"："+step.Detail)
		}
	}
	st.save()
	st.mu.Unlock()
	if len(failed) > 0 {
		s.logf("selftest %s failed: %s", id, strings.Join(failed, "；"))
		_ = s.raiseSystem(selfTestRef, "Vigil 自测没通过", "端到端自测有步骤失败，相关自动化可能没在工作", strings.Join(failed, "\n"))
		return
	}
	s.logf("selftest %s passed", id)
	_ = s.resolveSystem(selfTestRef, "自测已通过")
}

// selfTestStep runs one step, records it and returns the value it produced ("" on failure).
func (s *Server) selfTestStep(ctx context.Context, name string, fn func() (string, error)) string {
	start := s.Now()
	val, err := fn()
	step := SelfTestStep{Name: name, OK: err == nil, Took: s.Now().Sub(start).Round(time.Second).String(), Detail: "通过"}
	if err != nil {
		step.Detail = err.Error()
		val = ""
	}
	s.SelfTest.record(step)
	if val == "" && err == nil {
		val = "ok"
	}
	return val
}

// stepListen posts an instruction in the self-DM and waits for the watch to answer in its thread.
func (s *Server) stepListen(ctx context.Context, id string) (string, error) {
	st := s.SelfTest
	want := "自测收到 " + id
	ts, err := st.Slack.Post(ctx, st.Channel, fmt.Sprintf(selfTestMark+" %s】这是自动端到端自测：请只在这条消息的线程里回复「%s」，不要做别的事，也不要建待办。", id, want), "")
	if err != nil {
		return "", fmt.Errorf("发自测消息失败：%w", err)
	}
	if err := s.waitFor(ctx, func() (bool, error) { return threadHas(ctx, st, ts, want) }); err != nil {
		return ts, fmt.Errorf("%s 内线程里没有出现「%s」（%w）", st.Step, want, err)
	}
	return ts, nil
}

// stepTodo raises a to-do with a drafted reply, answers it with「send」and waits for it to be posted.
func (s *Server) stepTodo(ctx context.Context, id, thread string) error {
	st := s.SelfTest
	reply := "自测待办回复 " + id
	it, _, err := s.Inbox.Add(inbox.Item{TaskID: st.Task, Title: "Vigil 自测：待办发送", Detail: "自动自测，会自己结案",
		Reply: reply, Target: "slack:" + st.Channel + ":" + thread, Ref: selfTestRef + ":" + id})
	if err != nil {
		return fmt.Errorf("建待办失败：%w", err)
	}
	if _, err := s.AnswerTodo(it.ID, TodoSend, reply); err != nil {
		return fmt.Errorf("回答待办失败：%w", err)
	}
	err = s.waitFor(ctx, func() (bool, error) {
		cur, err := s.Inbox.Get(it.ID)
		if err != nil || cur.Status != inbox.StatusDone {
			return false, err
		}
		return threadHas(ctx, st, thread, reply)
	})
	if err != nil {
		cur, _ := s.Inbox.Get(it.ID)
		_, _ = s.Inbox.Close(it.ID, "自测超时，自动关闭")
		return fmt.Errorf("待办 %s 状态 %s，线程里没等到「%s」（%w）", it.ID, cur.Status, reply, err)
	}
	return nil
}

// stepJob hands a no-op background job to the task and waits for its run to finish OK.
func (s *Server) stepJob(ctx context.Context, id string) error {
	st := s.SelfTest
	job, err := newJobID()
	if err != nil {
		return err
	}
	s.Engine.Submit(runner.Request{TaskID: st.Task, Trigger: runner.ByJob, Job: job, JobTitle: "Vigil 自测 " + id,
		Input: "这是 Vigil 端到端自测：不要做任何外部操作（不发消息、不改文件、不建待办），直接结束本轮，summary 写「自测子任务 " + id + "」。"})
	var got store.RunRecord
	err = s.waitFor(ctx, func() (bool, error) {
		runs, err := s.Store.Runs(st.Task, 50)
		if err != nil {
			return false, err
		}
		for _, r := range runs {
			if r.Job == job && r.Status != store.StatusRunning {
				got = r
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("子任务 %s 没在 %s 内跑完（%w）", job, st.Step, err)
	}
	if got.Status != store.StatusOK {
		return fmt.Errorf("子任务 %s 结束状态 %s：%s", job, got.Status, firstNonEmpty(got.Error, got.Summary))
	}
	return nil
}

var errStepTimeout = errors.New("超时")

func (s *Server) waitFor(ctx context.Context, done func() (bool, error)) error {
	st := s.SelfTest
	deadline := time.Now().Add(st.Step)
	var last error
	for {
		ok, err := done()
		if ok {
			return nil
		}
		if err != nil {
			last = err
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("%w，最后一次检查出错：%v", errStepTimeout, last)
			}
			return errStepTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(st.Poll):
		}
	}
}

func threadHas(ctx context.Context, st *SelfTest, ts, text string) (bool, error) {
	msgs, err := st.Slack.Thread(ctx, st.Channel, ts)
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		// the instruction itself quotes the expected reply; only answers count
		if strings.Contains(m, text) && !strings.Contains(m, selfTestMark) {
			return true, nil
		}
	}
	return false, nil
}

func nonce() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return "t" + hex.EncodeToString(b[:])
}
