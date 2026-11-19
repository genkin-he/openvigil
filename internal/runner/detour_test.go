package runner

import (
	"strings"
	"testing"

	"vigil/internal/exec"
	"vigil/internal/resident"
	"vigil/internal/result"
	"vigil/internal/store"
	"vigil/internal/task"
)

const stuckScreen = "────────────────────\n Bash command\n │ deploy list --env prod\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"

// A round that finds the resident agent already stuck on a dialog runs in a fresh headless session
// instead of queueing behind the dialog; the resident session and its card are left alone.
func TestARoundThatFindsTheAgentStuckRunsInAFreshSession(t *testing.T) {
	h := newHarness(t, exec.Outcome{SessionID: "tmp", FinalText: okBlock, CostUSD: 0.1})
	h.r.out = &resident.Outcome{Blocked: stuckScreen, Unsent: true, SessionID: "rs"}
	tk := manual("r")
	tk.Mode = task.ModeResident
	h.put(t, tk)
	_, _ = h.s.UpdateState("r", func(st store.State) store.State { st.SessionID, st.SessionRuns = "rs", 3; return st })

	runs := h.run(Request{TaskID: "r", Trigger: ByManual, Input: "灰度发布了吗"})

	if len(h.h.reqs) != 1 {
		t.Fatalf("headless runs = %d, want the round detoured once", len(h.h.reqs))
	}
	req := h.h.reqs[0]
	if req.Task.Mode != task.ModeHeadless || req.ResumeSession != "" || !strings.Contains(req.Prompt, "灰度发布了吗") {
		t.Fatalf("detour request = mode %q resume %q", req.Task.Mode, req.ResumeSession)
	}
	r := runs[0]
	if r.Dialog || r.Status != store.StatusOK || !strings.Contains(r.Summary, "同步了 3 条") || !strings.Contains(r.Summary, DetourNote) {
		t.Fatalf("run = %+v", r)
	}
	st, _ := h.s.State("r")
	if st.SessionID != "rs" || st.SessionRuns != 3 {
		t.Fatalf("resident session overwritten: %+v", st)
	}
}

// The round that was running when the dialog came up still asks the owner about it.
func TestARoundStuckMidwayStillAsksAboutTheDialog(t *testing.T) {
	h := newHarness(t, exec.Outcome{FinalText: okBlock})
	h.r.out = &resident.Outcome{Blocked: stuckScreen, SessionID: "rs"}
	tk := manual("r")
	tk.Mode = task.ModeResident
	h.put(t, tk)

	runs := h.run(Request{TaskID: "r", Trigger: ByManual})

	if len(h.h.reqs) != 0 || !runs[0].Dialog || runs[0].Status != store.StatusNeedsYou {
		t.Fatalf("headless = %d, run = %+v", len(h.h.reqs), runs[0])
	}
}

// Once the owner answers the dialog, the agent finishes the round and reports it: the report is kept
// even though the run was already closed as waiting on the dialog.
func TestALateReportFillsInTheRoundCutShortByADialog(t *testing.T) {
	h := newHarness(t, exec.Outcome{})
	h.r.out = &resident.Outcome{Blocked: stuckScreen, SessionID: "rs"}
	tk := manual("r")
	tk.Mode = task.ModeResident
	h.put(t, tk)
	runs := h.run(Request{TaskID: "r", Trigger: ByManual})

	err := h.e.Report(runs[0].ID, result.Result{Status: result.OK, Summary: "回复了小林", Memory: []string{"小林问的是灰度"}})

	if err != nil {
		t.Fatal(err)
	}
	runs, _ = h.s.Runs("r", 0)
	if len(runs) != 1 || runs[0].Status != store.StatusOK || runs[0].Summary != "回复了小林" || runs[0].Dialog || runs[0].Question != "" {
		t.Fatalf("runs = %+v", runs)
	}
	if mem, _ := h.s.ReadDoc("r", store.DocMemory); !strings.Contains(mem, "小林问的是灰度") {
		t.Fatalf("memory = %q", mem)
	}
	if err := h.e.Report(runs[0].ID, result.Result{Status: result.OK, Summary: "再来一次"}); err == nil {
		t.Fatal("a second late report was accepted")
	}
}
