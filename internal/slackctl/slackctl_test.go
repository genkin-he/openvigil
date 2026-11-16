package slackctl

import "testing"

func TestParseTS(t *testing.T) {
	for _, in := range []string{`{"ts":"1.2","channel":"D1"}`, `{"ok":true,"message":{"ts":"1.2"}}`} {
		if ts, err := parseTS([]byte(in)); err != nil || ts != "1.2" {
			t.Fatalf("%s → %q %v", in, ts, err)
		}
	}
	if _, err := parseTS([]byte(`{"ok":true}`)); err == nil {
		t.Fatal("missing ts accepted")
	}
}

func TestPickReply(t *testing.T) {
	out, err := decodeMessages([]byte(`[{"ts":"1.0","user":"U1","text":"card"},{"ts":"1.5","user":"U2","text":"不用管"}]`))
	if err != nil || out[1].User != "U2" {
		t.Fatalf("decode: %+v %v", out, err)
	}
	if got, err := pickReply(out, "1.5"); err != nil || got != "不用管" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := pickReply(out, "9.9"); err == nil {
		t.Fatal("missing reply found")
	}
}
