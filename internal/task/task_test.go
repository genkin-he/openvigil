package task

import (
	"strings"
	"testing"
	"time"
)

func TestParseFillsDefaults(t *testing.T) {
	got, err := Parse([]byte("id: issue-sync\ntrigger:\n  kind: cron\n  cron: \"37 10-18 * * 1-5\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Name != "issue-sync" || got.Agent.Kind != "claude" || got.Session.Mode != SessionContinue || got.Mode != ModeHeadless || got.Agent.PermissionMode != "auto" {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if got.Run.Timeout.Std() != 20*time.Minute || got.Run.CatchUp != CatchUpOnce || got.Evolve != EvolvePropose {
		t.Fatalf("run defaults not applied: %+v", got.Run)
	}
}

func TestParseStreamDefaults(t *testing.T) {
	got, err := Parse([]byte("id: slack\ntrigger:\n  kind: stream\n  stream:\n    command: tail -f x\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Trigger.Stream.BatchWindow.Std() != 20*time.Second || got.Trigger.Stream.MaxBatch != 50 {
		t.Fatalf("stream defaults not applied: %+v", got.Trigger.Stream)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"path traversal id": "id: ../etc\ntrigger: {kind: manual}\n",
		"missing cron":      "id: a\ntrigger: {kind: cron}\n",
		"invalid cron":      "id: a\ntrigger: {kind: cron, cron: \"61 * * * *\"}\n",
		"every too short":   "id: a\ntrigger: {kind: every, every: 10s}\n",
		"once without at":   "id: a\ntrigger: {kind: once}\n",
		"unknown trigger":   "id: a\ntrigger: {kind: hourly}\n",
		"bad session":       "id: a\ntrigger: {kind: manual}\nsession: {mode: forever}\n",
		"bad evolve":        "id: a\ntrigger: {kind: manual}\nevolve: yolo\n",
		"bad ignore regex":  "id: a\ntrigger: {kind: stream, stream: {command: x, ignore: [\"(\"]}}\n",
		"bad mode":          "id: a\nmode: daemon\ntrigger: {kind: manual}\n",
		"headless codex":    "id: a\nagent: {kind: codex}\ntrigger: {kind: manual}\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestWithDefaultsDoesNotMutateInput(t *testing.T) {
	in := Task{ID: "a", Trigger: Trigger{Kind: TriggerStream, Stream: &Stream{Command: "x"}}}
	_ = WithDefaults(in)
	if in.Trigger.Stream.MaxBatch != 0 || in.Name != "" {
		t.Fatalf("input mutated: %+v", in)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	orig, err := Parse([]byte("id: a\nname: 测试\ntrigger: {kind: every, every: 30m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "every: 30m0s") {
		t.Fatalf("duration not human readable:\n%s", data)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("reparse: %v\n%s", err, data)
	}
	if back.Trigger.Every != orig.Trigger.Every || back.Name != "测试" {
		t.Fatalf("round trip mismatch: %+v vs %+v", back, orig)
	}
}

func TestResidentAllowsOtherAgents(t *testing.T) {
	if _, err := Parse([]byte("id: a\nmode: resident\nagent: {kind: codex}\ntrigger: {kind: manual}\n")); err != nil {
		t.Fatalf("resident codex should be valid: %v", err)
	}
}

func TestIgnoreMatcher(t *testing.T) {
	m, err := IgnoreMatcher([]string{"^LISTENER connected ", "^OUTBOX ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if !m("LISTENER connected {}") || m("LISTENER reconnected {}") || m("SLACK D1:1 | LISTENER connected ") {
		t.Fatal("matcher wrong")
	}
}
