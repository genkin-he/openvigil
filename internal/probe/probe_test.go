package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRun answers each command line with canned output.
type fakeRun struct {
	out   map[string]string
	calls []string
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	for prefix, out := range f.out {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), nil
		}
	}
	return nil, errors.New("unexpected: " + line)
}

func TestMergeRequestStateFromItsLink(t *testing.T) {
	cases := map[string]struct {
		state   string
		settled bool
		outcome string
	}{
		"merged": {"merged", true, "MR 已合并"},
		"closed": {"closed", true, "MR 已关闭（没合）"},
		"open":   {"opened", false, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeRun{out: map[string]string{"glab mr view 2298 -R craft/lb-cms -F json": `{"iid":2298,"state":"` + c.state + `"}`}}
			p, _ := New(f.run, nil)
			got, err := p.Check(context.Background(), "https://gitlab.example.com/craft/lb-cms/-/merge_requests/2298", "gitlab:lb-cms:2298")
			if err != nil || got.Settled != c.settled || got.Outcome != c.outcome || !got.Known {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

// A lookup from config.yaml: the command gets the link and the ref as arguments and says what it found.
func TestAConfiguredLookupRunsItsCommandWithTheLinkAndRef(t *testing.T) {
	ticket := Custom{Name: "ticket", Match: `^(https://tickets\.example\.com/\d+|ticket:\d+)$`, Command: "~/bin/ticket-probe --quiet"}
	cases := map[string]struct {
		link, ref, out string
		settled        bool
		outcome        string
	}{
		"done by link": {"https://tickets.example.com/7", "", "settled 工单已执行\n", true, "工单已执行"},
		"pending":      {"https://tickets.example.com/7", "", "pending\n", false, ""},
		"matched ref":  {"", "ticket:7", "settled 已取消", true, "已取消"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeRun{out: map[string]string{"/bin/bash -c ~/bin/ticket-probe --quiet \"$1\" \"$2\" probe " + c.link + " " + c.ref: c.out}}
			k, err := New(f.run, []Custom{ticket})
			if err != nil {
				t.Fatal(err)
			}
			got, err := k.Check(context.Background(), c.link, c.ref)
			if err != nil || !got.Known || got.Settled != c.settled || got.Outcome != c.outcome {
				t.Fatalf("got %+v, %v (calls %v)", got, err, f.calls)
			}
		})
	}
}

func TestAConfiguredLookupWithUnreadableOutputIsAnError(t *testing.T) {
	f := &fakeRun{out: map[string]string{"/bin/bash": "{\"status\":\"DONE\"}"}}
	k, _ := New(f.run, []Custom{{Name: "t", Match: `^ticket:`, Command: "x"}})
	if got, err := k.Check(context.Background(), "", "ticket:1"); err == nil || got.Settled {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestABadPatternIsRejected(t *testing.T) {
	for _, c := range []Custom{{Name: "a", Match: "(", Command: "x"}, {Name: "b", Match: "x"}, {Name: "", Match: "x", Command: "x"}} {
		if _, err := New(nil, []Custom{c}); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestUnknownThingsAreNotChecked(t *testing.T) {
	f := &fakeRun{}
	for _, c := range [][2]string{
		{"https://example.com/x", "other:1"},
		{"https://gitlab.example.com/craft/lb-cms/-/issues/3", ""},
		{"", "ticket:abc"},
		{"", "slack:C1:1.0"},
	} {
		k, _ := New(f.run, []Custom{{Name: "t", Match: `^ticket:\d+$`, Command: "x"}})
		got, err := k.Check(context.Background(), c[0], c[1])
		if err != nil || got.Known || got.Settled {
			t.Fatalf("%v: got %+v, %v", c, got, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("ran %v", f.calls)
	}
}

func TestAFailedCommandIsAnErrorNotASettlement(t *testing.T) {
	f := &fakeRun{out: map[string]string{}}
	k, _ := New(f.run, nil)
	got, err := k.Check(context.Background(), "https://gitlab.example.com/craft/lb-cms/-/merge_requests/1", "")
	if err == nil || got.Settled {
		t.Fatalf("got %+v, %v", got, err)
	}
}
