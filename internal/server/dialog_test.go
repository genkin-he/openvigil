package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"vigil/internal/resident"
	"vigil/internal/runner"
	"vigil/internal/store"
	"vigil/internal/task"
)

type fakeDialogs struct {
	err     error
	pressed []string
}

func (f *fakeDialogs) AnswerDialog(_ context.Context, t task.Task, shown, answer string) error {
	if f.err != nil {
		return f.err
	}
	f.pressed = append(f.pressed, t.ID+"|"+shown+"|"+answer)
	return nil
}

func dialogRun(t *testing.T, s *Server, id, input string, at int64) {
	t.Helper()
	if err := s.Store.AppendRun(store.RunRecord{ID: id, TaskID: "slack", Trigger: runner.ByStream, Status: store.StatusNeedsYou,
		Dialog: true, Summary: "Bash command：回报本轮结果", Question: "agent 在等你回答「Do you want to proceed?」",
		Options: []string{"Yes", "No"}, Input: input, StartedAt: time.Unix(at, 0), EndedAt: time.Unix(at+1, 0)}); err != nil {
		t.Fatal(err)
	}
}

func TestAnsweringADialogPressesItAndRerunsTheRoundsStuckBehindIt(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	f := &fakeDialogs{}
	s.Dialogs = f
	dialogRun(t, s, "r1", "小林：在吗", 100)        // in progress when the dialog came up: carries on by itself
	dialogRun(t, s, "r2", "小周：sitemap 呢", 200) // never reached the agent
	dialogRun(t, s, "r3", "小周：sitemap 呢", 300) // the engine's retry of r2
	dialogRun(t, s, "r4", "小吴：发版了吗", 400)

	out, err := s.Answer("slack", "Yes")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.pressed) != 1 || f.pressed[0] != "slack|Bash command：回报本轮结果|Yes" {
		t.Fatalf("pressed = %q", f.pressed)
	}
	s.Engine.Wait()
	var reruns []string // the engine may fold queued inputs into one round
	runs, _ := s.Store.Runs("slack", 0)
	for _, r := range runs {
		if r.Trigger == runner.ByRetry {
			reruns = append(reruns, r.Input)
		}
		if r.Trigger == runner.ByAnswer {
			t.Fatalf("an answer to a dialog started a new round: %+v", r)
		}
	}
	if out["rerun"] != 2 || len(reruns) == 0 || !strings.Contains(strings.Join(reruns, "|"), "sitemap") ||
		!strings.Contains(strings.Join(reruns, "|"), "发版") || strings.Contains(strings.Join(reruns, "|"), "在吗") {
		t.Fatalf("out = %v reruns = %q", out, reruns)
	}
	if _, err := s.Answer("slack", "Yes"); !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("second answer: %v", err)
	}
}

func TestADialogThatChangedIsNotPressed(t *testing.T) {
	s, h := newServer(t)
	do(h, "POST", "/api/tasks", applyJSON("id: slack\nname: Slack\nenabled: true\ntrigger: {kind: manual}\n"), true)
	s.Dialogs = &fakeDialogs{err: resident.ErrDialogChanged}
	dialogRun(t, s, "r1", "小林：在吗", 100)
	_, err := s.Answer("slack", "Yes")
	var he *httpError
	if !errors.As(err, &he) || he.code != http.StatusConflict {
		t.Fatalf("err = %v", err)
	}
	if st, _ := s.Store.State("slack"); st.AnsweredRun != "" {
		t.Fatal("marked answered although nothing was pressed")
	}
}
