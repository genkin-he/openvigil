package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"vigil/internal/exec"
	"vigil/internal/forge"
	"vigil/internal/herdr"
	"vigil/internal/runner"
	"vigil/internal/store"
)

const token = "0123456789abcdef0123456789abcdef"

type okHeadless struct{}

func (okHeadless) Run(context.Context, exec.Request) exec.Outcome {
	return exec.Outcome{FinalText: "```json\n{\"status\":\"ok\",\"summary\":\"跑完了\"}\n```", SessionID: "s1"}
}

func newServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := runner.New(context.Background(), runner.Config{Store: st, Headless: okHeadless{}})
	s := &Server{Store: st, Engine: e, Token: token, Addr: "127.0.0.1:7788", Forge: forge.Forge{H: &noHerdr{}},
		Web: fstest.MapFS{"index.html": {Data: []byte(`<meta name="token" content="__VIGIL_TOKEN__">`)}}}
	return s, s.Handler()
}

func do(h http.Handler, method, path, body string, auth bool) (*httptest.ResponseRecorder, envelope) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7788"
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var env envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec, env
}

func applyJSON(yaml string) string {
	b, _ := json.Marshal(map[string]any{"yaml": yaml, "brief": "需求"})
	return string(b)
}

func TestAuthAndHostGuard(t *testing.T) {
	_, h := newServer(t)
	if rec, _ := do(h, "GET", "/api/overview", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/api/overview", nil)
	req.Host = "evil.example:7788"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding host allowed: %d", rec.Code)
	}
	page, _ := do(h, "GET", "/", "", false)
	if !strings.Contains(page.Body.String(), token) {
		t.Fatal("token not injected into console")
	}
}

func TestApplyRunAndDetail(t *testing.T) {
	s, h := newServer(t)
	rec, env := do(h, "POST", "/api/tasks", applyJSON("id: demo\nname: 演示\ntrigger: {kind: manual}\n"), true)
	if rec.Code != http.StatusOK || !env.OK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if brief, _ := s.Store.ReadDoc("demo", store.DocBrief); brief != "需求" {
		t.Fatalf("brief = %q", brief)
	}
	if rec, _ := do(h, "POST", "/api/tasks/demo/run", `{"input":"现在跑"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("run: %d", rec.Code)
	}
	s.Engine.Wait()
	rec, env = do(h, "GET", "/api/tasks/demo", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "跑完了") {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(h, "POST", "/api/tasks", applyJSON("id: ../x\ntrigger: {kind: manual}\n"), true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id accepted: %d", rec.Code)
	}
	if rec, _ := do(h, "GET", "/api/tasks/nope", "", true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing task: %d", rec.Code)
	}
}

func TestAmendHonoursEvolvePolicy(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: p\ntrigger: {kind: manual}\nevolve: propose\n"), true)
	do(h, "POST", "/api/tasks", applyJSON("id: o\ntrigger: {kind: manual}\nevolve: off\n"), true)
	do(h, "POST", "/api/tasks/p/amend", `{"content":"新做法","reason":"更快"}`, true)
	if ps, _ := s.Store.Proposals("p"); len(ps) != 1 {
		t.Fatalf("proposal not saved: %+v", ps)
	}
	if rb, _ := s.Store.ReadDoc("p", store.DocRunbook); rb != "" {
		t.Fatal("propose must not apply directly")
	}
	if rec, _ := do(h, "POST", "/api/tasks/o/amend", `{"content":"x"}`, true); rec.Code != http.StatusForbidden {
		t.Fatalf("evolve off allowed amend: %d", rec.Code)
	}
}

func TestDoneForUnknownRun(t *testing.T) {
	_, h := newServer(t)
	rec, _ := do(h, "POST", "/api/runs/r20261110-101010-abcdef/done", `{"status":"ok","summary":"x"}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown run: %d", rec.Code)
	}
	rec, _ = do(h, "POST", "/api/runs/r1/done", `{"status":"maybe","summary":"x"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid result: %d", rec.Code)
	}
}

func TestParseTranscript(t *testing.T) {
	src := `{"type":"assistant","message":{"content":[{"type":"text","text":"先看看"},{"type":"tool_use","name":"Bash","input":{"command":"ls -la"}}]}}
{"type":"result","result":"done"}`
	got := parseTranscript(strings.NewReader(src))
	if len(got) != 3 || got[1].Text != "Bash ls -la" || got[2].Kind != "result" {
		t.Fatalf("transcript = %+v", got)
	}
}

func TestPauseResumeDocsAnswerAndRemove(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: d\nenabled: true\ntrigger: {kind: every, every: 5m}\n"), true)
	_, _ = s.Store.UpdateState("d", func(st store.State) store.State { st.LastScheduledAt = s.Now(); return st })

	if rec, _ := do(h, "POST", "/api/tasks/d/pause", "", true); rec.Code != http.StatusOK {
		t.Fatalf("pause: %d", rec.Code)
	}
	if tk, _ := s.Store.Get("d"); tk.Enabled {
		t.Fatal("still enabled")
	}
	do(h, "POST", "/api/tasks/d/resume", "", true)
	st, _ := s.Store.State("d")
	if tk, _ := s.Store.Get("d"); !tk.Enabled || !st.LastScheduledAt.IsZero() {
		t.Fatalf("resume must enable and restart the timetable: %+v", st)
	}

	if rec, _ := do(h, "PUT", "/api/tasks/d/docs/runbook", `{"text":"新做法"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("put doc: %d", rec.Code)
	}
	if rec, _ := do(h, "PUT", "/api/tasks/d/docs/..%2Ftask", `{"text":"x"}`, true); rec.Code == http.StatusOK {
		t.Fatal("unknown doc accepted")
	}
	if rec, _ := do(h, "POST", "/api/tasks/d/answer", `{"text":"  "}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty answer: %d", rec.Code)
	}
	do(h, "POST", "/api/tasks/d/answer", `{"text":"A"}`, true)
	s.Engine.Wait()
	runs, _ := s.Store.Runs("d", 0)
	if len(runs) != 1 || !strings.Contains(runs[0].Input, "我对上一轮问题的回答：A") {
		t.Fatalf("answer run = %+v", runs)
	}
	focusRec, _ := do(h, "GET", "/api/tasks/d?run="+runs[0].ID, "", true)
	if !strings.Contains(focusRec.Body.String(), runs[0].ID) {
		t.Fatalf("detail with run focus: %s", focusRec.Body)
	}
	do(h, "POST", "/api/tasks/d/memory", `{"text":"纠错：记住这个"}`, true)
	if mem, _ := s.Store.ReadDoc("d", store.DocMemory); !strings.Contains(mem, "纠错：记住这个") {
		t.Fatalf("memory = %q", mem)
	}
	do(h, "POST", "/api/tasks/d/memory", `{"text":"[demo-db] 订单表叫 orders"}`, true)
	if topics, _ := s.Store.Knowledge("d"); len(topics) != 1 || topics[0].Topic != "demo-db" {
		t.Fatalf("a tagged fact did not go to knowledge: %+v", topics)
	}
	if rec, _ := do(h, "DELETE", "/api/tasks/d", "", true); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec, _ := do(h, "GET", "/api/overview", "", true); !strings.Contains(rec.Body.String(), `"tasks":[]`) {
		t.Fatalf("overview after delete: %s", rec.Body)
	}
}

func TestRunLogRejectsBadRunID(t *testing.T) {
	_, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: l\ntrigger: {kind: manual}\n"), true)
	if rec, _ := do(h, "GET", "/api/tasks/l/runs/..%2F..%2Ftoken/log", "", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal run id: %d", rec.Code)
	}
	if rec, env := do(h, "GET", "/api/tasks/l/runs/r20261110-101010-abcdef/log", "", true); rec.Code != http.StatusOK || !env.OK {
		t.Fatalf("missing log should be empty: %d", rec.Code)
	}
}

func TestDetailOfNewTaskHasEmptyLists(t *testing.T) {
	_, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: fresh\ntrigger: {kind: manual}\n"), true)
	rec, _ := do(h, "GET", "/api/tasks/fresh", "", true)
	body := rec.Body.String()
	if !strings.Contains(body, `"runs":[]`) || !strings.Contains(body, `"proposals":[]`) {
		t.Fatalf("lists must be [] not null: %s", body)
	}
}

func TestResidentRunLogComesFromTranscript(t *testing.T) {
	s, h := newServer(t)
	s.ClaudeProjects = t.TempDir()
	do(h, "POST", "/api/tasks", applyJSON("id: res\nmode: resident\ntrigger: {kind: manual}\n"), true)
	sid := "1113c523-18db-4c0c-9286-8770c6aa5b99"
	start := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	runA := store.RunRecord{ID: "r20261110-120000-aaaaaa", TaskID: "res", Status: store.StatusOK, SessionID: sid, StartedAt: start, EndedAt: start.Add(10 * time.Second)}
	runB := store.RunRecord{ID: "r20261110-120030-bbbbbb", TaskID: "res", Status: store.StatusOK, SessionID: sid, StartedAt: start.Add(30 * time.Second), EndedAt: start.Add(40 * time.Second)}
	_ = s.Store.AppendRun(runA)
	_ = s.Store.AppendRun(runB)
	dir := filepath.Join(s.ClaudeProjects, "-Users-x")
	_ = os.MkdirAll(dir, 0o700)
	at := func(sec int) string { return start.Add(time.Duration(sec) * time.Second).Format(time.RFC3339Nano) }
	lines := []string{
		`{"type":"user","timestamp":"` + at(-30) + `","message":{"content":"earlier run"}}`,
		`{"type":"user","timestamp":"` + at(1) + `","message":{"content":"<pasted_content id=\"1\">\n【Vigil 值守】任务「res」\nmore"}}`,
		`{"type":"assistant","timestamp":"` + at(3) + `","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ps -p 1"}}]}}`,
		`{"type":"assistant","timestamp":"` + at(20) + `","message":{"content":[{"type":"text","text":"收尾"}]}}`,
		`{"type":"assistant","timestamp":"` + at(31) + `","message":{"content":[{"type":"text","text":"next run"}]}}`,
	}
	_ = os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(strings.Join(lines, "\n")), 0o600)

	rec, _ := do(h, "GET", "/api/tasks/res/runs/"+runA.ID+"/log", "", true)
	body := rec.Body.String()
	for _, want := range []string{"【Vigil 值守】任务「res」", "Bash ps -p 1", "收尾"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "earlier run") || strings.Contains(body, "next run") || strings.Contains(body, "more") {
		t.Fatalf("window leaked other runs: %s", body)
	}
}

func TestTranscriptPathRejectsBadSessionID(t *testing.T) {
	if transcriptPath(t.TempDir(), "../../etc/passwd") != "" {
		t.Fatal("bad session id accepted")
	}
}

func TestProposalFlowAndForgeGuards(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: pr\ntrigger: {kind: manual}\nevolve: propose\n"), true)
	_ = s.Store.WriteDoc("pr", store.DocRunbook, "v1")
	do(h, "POST", "/api/tasks/pr/amend", `{"content":"v2","reason":"更稳"}`, true)
	ps, _ := s.Store.Proposals("pr")
	if rec, _ := do(h, "POST", "/api/tasks/pr/proposals/"+ps[0].Name, `{"approve":true}`, true); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d", rec.Code)
	}
	if rb, _ := s.Store.ReadDoc("pr", store.DocRunbook); rb != "v2" {
		t.Fatalf("runbook = %q", rb)
	}
	if rec, _ := do(h, "POST", "/api/tasks/pr/proposals/..%2Fx", `{"approve":true}`, true); rec.Code == http.StatusOK {
		t.Fatal("bad proposal name accepted")
	}
	do(h, "POST", "/api/tasks", applyJSON("id: au\ntrigger: {kind: manual}\nevolve: auto\n"), true)
	do(h, "POST", "/api/tasks/au/amend", `{"content":"直接生效"}`, true)
	if rb, _ := s.Store.ReadDoc("au", store.DocRunbook); rb != "直接生效" {
		t.Fatalf("auto evolve runbook = %q", rb)
	}
	if rec, _ := do(h, "POST", "/api/forge", `{"idea":"  "}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty idea: %d", rec.Code)
	}
	if rec, _ := do(h, "POST", "/api/forge/bash/say", `{"text":"x"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-vigil session accepted: %d", rec.Code)
	}
}

// noHerdr satisfies forge.Herdr for tests that never reach herdr.
type noHerdr struct{}

func (noHerdr) AgentGet(context.Context, string) (herdr.Agent, error) {
	return herdr.Agent{}, herdr.ErrNoAgent
}
func (noHerdr) AgentList(context.Context) ([]herdr.Agent, error) { return nil, nil }
func (noHerdr) CreateWorkspace(context.Context, string, string) (string, error) {
	return "", errors.New("no herdr")
}
func (noHerdr) StartAgent(context.Context, string, string, string, []string) error { return nil }
func (noHerdr) Prompt(context.Context, string, string) error                       { return nil }
func (noHerdr) Read(context.Context, string, int) (string, error)                  { return "", nil }
func (noHerdr) SendKeys(context.Context, string, ...string) error                  { return nil }
func (noHerdr) WorkspaceList(context.Context) ([]herdr.Workspace, error)           { return nil, nil }
func (noHerdr) FocusWorkspace(context.Context, string) error                       { return nil }
func (noHerdr) CloseWorkspace(context.Context, string) error                       { return nil }
func (noHerdr) PaneRun(context.Context, string, string) error                      { return nil }

func TestAnswerAllowsDeniedOnceAndRemembersDecision(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: dn\ntrigger: {kind: manual}\n"), true)
	_ = s.Store.AppendRun(store.RunRecord{ID: "r20261110-120000-aaaaaa", TaskID: "dn", Status: store.StatusNeedsYou,
		Question: "这一轮有操作被权限拦下了", Denied: []string{"Bash(touch:*)"}, StartedAt: time.Now()})
	_, env := do(h, "POST", "/api/tasks/dn/answer", `{"text":"A: 允许这些操作"}`, true)
	if data, _ := json.Marshal(env.Data); !strings.Contains(string(data), "Bash(touch:*)") {
		t.Fatalf("answer A did not allow: %s", data)
	}
	s.Engine.Wait()
	mem, _ := s.Store.ReadDoc("dn", store.DocMemory)
	if !strings.Contains(mem, "决策：这一轮有操作被权限拦下了 → A: 允许这些操作") {
		t.Fatalf("decision not remembered: %q", mem)
	}
	cases := map[string]int{
		"A": AllowOnce, "允许": AllowOnce, "可以，放行": AllowOnce,
		"B": AllowAlways, "永久放行": AllowAlways, "以后都允许": AllowAlways,
		"C": AllowDeny, "不允许": AllowDeny, "拒绝": AllowDeny,
		"先别动": AllowUnknown, "嗯": AllowUnknown,
	}
	for ans, want := range cases {
		if got := AllowDecision(ans); got != want {
			t.Fatalf("AllowDecision(%q) = %d, want %d", ans, got, want)
		}
	}
}

func TestAnswerAlwaysAllowsAndRejectsUnclear(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: pa\ntrigger: {kind: manual}\n"), true)
	_ = s.Store.AppendRun(store.RunRecord{ID: "r20261110-120000-bbbbbb", TaskID: "pa", Status: store.StatusNeedsYou,
		Question: "这一轮有操作被权限拦下了", Denied: []string{"Bash(touch:*)"}, StartedAt: time.Now()})
	if rec, _ := do(h, "POST", "/api/tasks/pa/answer", `{"text":"嗯"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("unclear answer must be rejected, got %d", rec.Code)
	}
	if rec, _ := do(h, "POST", "/api/tasks/pa/answer", `{"text":"永久放行"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("always: %d", rec.Code)
	}
	s.Engine.Wait()
	tk, _ := s.Store.Get("pa")
	if len(tk.Agent.AllowedTools) != 1 || tk.Agent.AllowedTools[0] != "Bash(touch:*)" {
		t.Fatalf("allowed_tools = %v", tk.Agent.AllowedTools)
	}
}

func TestSecondAnswerToSameQuestionIsRejected(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: once\ntrigger: {kind: manual}\n"), true)
	_ = s.Store.AppendRun(store.RunRecord{ID: "r20261110-120000-cccccc", TaskID: "once", Status: store.StatusNeedsYou,
		Question: "要不要做", StartedAt: time.Now()})
	if rec, _ := do(h, "POST", "/api/tasks/once/answer", `{"text":"B"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("first answer: %d", rec.Code)
	}
	rec, _ := do(h, "POST", "/api/tasks/once/answer", `{"text":"B"}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second answer to the same question: %d", rec.Code)
	}
	if _, err := s.Answer("once", "A"); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("Answer duplicate err = %v", err)
	}
	s.Engine.Wait()
}

func TestAnsweredQuestionLeavesNeedsYou(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: aq\ntrigger: {kind: manual}\n"), true)
	_ = s.Store.AppendRun(store.RunRecord{ID: "r20261110-120000-dddddd", TaskID: "aq", Status: store.StatusNeedsYou,
		Question: "做到什么范围", StartedAt: time.Now()})
	before := s.view(must(s.Store.Get("aq")), nil, time.Now())
	if before.NeedsYou == nil || before.Answered != nil {
		t.Fatalf("before answer: %+v", before)
	}
	if _, err := s.Store.UpdateState("aq", func(st store.State) store.State {
		st.AnsweredRun, st.AnsweredText = "r20261110-120000-dddddd", "B"
		return st
	}); err != nil {
		t.Fatal(err)
	}
	after := s.view(must(s.Store.Get("aq")), nil, time.Now())
	if after.NeedsYou != nil || after.Answered == nil || after.Answered.Answer != "B" {
		t.Fatalf("after answer: needs=%v answered=%+v", after.NeedsYou, after.Answered)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestForgeStagesAndClose(t *testing.T) {
	s, h := newServer(t)
	name := "forge-abc123"
	if err := s.saveForge(forgeRecord{Name: name, Idea: "每天提醒", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stage := func() string {
		_, env := do(h, "GET", "/api/forge/"+name+"/status", "", true)
		m, _ := env.Data.(map[string]any)
		return fmt.Sprint(m["stage"])
	}
	if got := stage(); got != StageAsking {
		t.Fatalf("stage = %s", got)
	}
	body, _ := json.Marshal(map[string]any{"yaml": "id: remind\ntrigger: {kind: manual}\n", "forge": name})
	do(h, "POST", "/api/tasks", string(body), true)
	if got := stage(); got != StageDrafted {
		t.Fatalf("after apply stage = %s", got)
	}
	_ = s.Store.AppendRun(store.RunRecord{ID: "r20261110-120000-eeeeee", TaskID: "remind", Status: store.StatusOK, StartedAt: time.Now()})
	if got := stage(); got != StageTrialed {
		t.Fatalf("after trial stage = %s", got)
	}
	do(h, "POST", "/api/tasks/remind/resume", "", true)
	if got := stage(); got != StageLive {
		t.Fatalf("after resume stage = %s", got)
	}
	if rec, _ := do(h, "POST", "/api/forge/"+name+"/close", "", true); rec.Code != http.StatusOK {
		t.Fatalf("close: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(s.Store.Root(), "forges", name+".json")); !os.IsNotExist(err) {
		t.Fatal("forge record not removed")
	}
	if rec, _ := do(h, "GET", "/api/forge/..%2Fx/status", "", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name accepted: %d", rec.Code)
	}
}

func TestTaskDetailShowsMemorySizeAndKnowledge(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: d\nname: D\ntrigger: {kind: manual}\n"), true)
	_ = s.Store.AppendMemory("d", []string{"纠错：先查再答", "[monitor] 查日志要带集群", "[monitor] 没有 --since"}, s.Now())

	_, env := do(h, "GET", "/api/tasks/d", "", true)
	detail := decodeData[struct {
		Memory struct {
			Bytes     int                    `json:"bytes"`
			Cap       int                    `json:"cap"`
			Knowledge []store.KnowledgeTopic `json:"knowledge"`
		} `json:"memory"`
	}](t, env.Data)
	m := detail.Memory
	if m.Bytes == 0 || m.Cap != store.MemoryCap || len(m.Knowledge) != 1 || m.Knowledge[0].Topic != "monitor" || m.Knowledge[0].Entries != 2 {
		t.Fatalf("memory = %+v", m)
	}

	_, env = do(h, "GET", "/api/tasks/d/knowledge/monitor", "", true)
	if got := decodeData[map[string]string](t, env.Data); !strings.Contains(got["content"], "没有 --since") {
		t.Fatalf("topic = %+v", got)
	}
	for _, bad := range []string{"nope", "..%2Fmemory", "Monitor"} {
		if rec, _ := do(h, "GET", "/api/tasks/d/knowledge/"+bad, "", true); rec.Code == 200 {
			t.Errorf("topic %q served", bad)
		}
	}
}
