package health

import (
	"context"
	"errors"
	"testing"
)

type fakeShell struct {
	fail map[string]int // command → remaining failures
	ran  []string
}

func (f *fakeShell) run(_ context.Context, cmd string) (string, error) {
	f.ran = append(f.ran, cmd)
	if f.fail[cmd] > 0 {
		f.fail[cmd]--
		return "deploy: ❌ needs sign-in", errors.New("exit status 1")
	}
	return "ok", nil
}

func TestCheckerRaisesAndResolves(t *testing.T) {
	sh := &fakeShell{fail: map[string]int{"check-a": 2}} // fails on the check and on the retry
	raised, resolved := map[string]string{}, map[string]bool{}
	k := &Checker{
		Checks:  []Check{{Name: "a", Command: "check-a"}, {Name: "b", Command: "check-b"}},
		Exec:    sh.run,
		Raise:   func(c Check, out string) error { raised[c.Name] = out; return nil },
		Resolve: func(c Check) error { resolved[c.Name] = true; return nil },
	}
	res := k.Run(context.Background())
	if res[0].OK || !res[1].OK || raised["a"] == "" || !resolved["b"] {
		t.Fatalf("first run: %+v raised=%v resolved=%v", res, raised, resolved)
	}
	res = k.Run(context.Background())
	if !res[0].OK || !resolved["a"] {
		t.Fatalf("a should recover: %+v", res)
	}
	if last := k.Last(); !last["a"].OK {
		t.Fatalf("last = %+v", last)
	}
}

func TestCheckerTriesFixFirst(t *testing.T) {
	sh := &fakeShell{fail: map[string]int{"check": 2}}
	raised := false
	k := &Checker{
		Checks: []Check{{Name: "slack", Command: "check", Fix: "repair"}},
		Exec:   sh.run,
		Raise:  func(Check, string) error { raised = true; return nil },
	}
	res := k.Run(context.Background())
	if !res[0].OK || !res[0].Fixed || raised {
		t.Fatalf("fix should have repaired it without a to-do: %+v raised=%v", res, raised)
	}
	if len(sh.ran) != 4 || sh.ran[2] != "repair" {
		t.Fatalf("ran = %v", sh.ran)
	}
}

func TestDefaultsAreComplete(t *testing.T) {
	if len(Defaults(false)) != 2 || len(Defaults(true)) != 3 {
		t.Fatalf("defaults: %d without Slack, %d with", len(Defaults(false)), len(Defaults(true)))
	}
	for _, c := range Defaults(true) {
		if c.Name == "" || c.Command == "" || c.Hint == "" {
			t.Fatalf("incomplete default check: %+v", c)
		}
	}
}

func TestCheckerIgnoresFailuresFromShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	raised := false
	k := &Checker{
		Checks: []Check{{Name: "a", Command: "check", Fix: "repair"}},
		Exec: func(context.Context, string) (string, error) {
			cancel() // the daemon stops while the command runs
			return "", errors.New("signal: killed")
		},
		Raise: func(Check, string) error { raised = true; return nil },
	}
	if res := k.Run(ctx); len(res) != 0 || raised {
		t.Fatalf("shutdown produced a result %+v raised=%v", res, raised)
	}
}

// 11-24 19:01 glab's API call hit EOF once (a network blip, the token was fine) and the owner got a
// "login expired" card; a minute later it passed. A check that passes on a second try is not raised.
func TestCheckerRetriesOnceBeforeRaising(t *testing.T) {
	sh := &fakeShell{fail: map[string]int{"check": 1}}
	raised := false
	k := &Checker{
		Checks: []Check{{Name: "glab", Command: "check", Fix: "repair"}},
		Exec:   sh.run,
		Raise:  func(Check, string) error { raised = true; return nil },
	}
	res := k.Run(context.Background())
	if !res[0].OK || res[0].Fixed || raised {
		t.Fatalf("a blip should pass on the retry without a to-do: %+v raised=%v", res, raised)
	}
	if len(sh.ran) != 2 || sh.ran[1] != "check" {
		t.Fatalf("the retry must run the check again, not the fix: %v", sh.ran)
	}
}
