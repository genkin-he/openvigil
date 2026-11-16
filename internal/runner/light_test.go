package runner

import (
	"testing"
	"time"

	"vigil/internal/exec"
	"vigil/internal/store"
	"vigil/internal/stream"
	"vigil/internal/task"
)

func TestLightBatchesUseTheCheapModel(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock, SessionID: "light-1"})
	tk := task.Task{ID: "w", Enabled: true, Mode: task.ModeResident,
		Trigger: task.Trigger{Kind: task.TriggerStream, Stream: &task.Stream{Command: "true",
			Light: &task.Light{Model: "haiku", Match: []string{`\| (好的|谢谢)$`}}}}}
	h.put(t, tk)
	if _, err := h.s.UpdateState("w", func(st store.State) store.State { st.SessionID = "main"; st.SessionRuns = 2; return st }); err != nil {
		t.Fatal(err)
	}
	dir, _ := h.s.SpoolDir("w")
	sp, _ := stream.OpenSpool(dir)

	_, _ = sp.Append("SLACK D1:1 thread=- | 好的", time.Now())
	_, _ = sp.Append("SLACK D2:2 thread=- | 谢谢", time.Now())
	runs := h.run(Request{TaskID: "w", Trigger: ByStream})
	if len(h.h.reqs) != 1 || h.h.reqs[0].Task.Agent.Model != "haiku" || h.h.reqs[0].ResumeSession != "" {
		t.Fatalf("light batch should run headless on haiku, fresh: %+v", h.h.reqs)
	}
	if runs[0].Model != "haiku" || runs[0].Status != store.StatusOK {
		t.Fatalf("run = %+v", runs[0])
	}
	if st, _ := h.s.State("w"); st.SessionID != "main" || st.SessionRuns != 2 {
		t.Fatalf("the resident session must stay untouched: %+v", st)
	}

	// One event that needs thought sends the whole batch to the task's own agent.
	_, _ = sp.Append("SLACK D1:3 thread=- | 好的", time.Now())
	_, _ = sp.Append("SLACK D3:4 thread=- | 订单为什么没成交？", time.Now())
	runs = h.run(Request{TaskID: "w", Trigger: ByStream})
	if len(h.h.reqs) != 1 || runs[0].Model != "" {
		t.Fatalf("mixed batch went to the light lane: reqs=%d run=%+v", len(h.h.reqs), runs[0])
	}
}
