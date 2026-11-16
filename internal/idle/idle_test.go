package idle

import (
	"context"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	d, err := Parse(`    | |     "HIDIdleTime" = 300000000000`)
	if err != nil || d != 5*time.Minute {
		t.Fatalf("Parse = %v, %v", d, err)
	}
	if _, err := Parse("nothing"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDurationOnThisMachine(t *testing.T) {
	if _, err := Duration(context.Background()); err != nil {
		t.Skipf("ioreg unavailable: %v", err)
	}
}

func TestParseLocked(t *testing.T) {
	cases := map[string]bool{
		`"IOConsoleLocked" = No` + "\n" + `{"kCGSSessionOnConsoleKey"=Yes}`: false,
		`"IOConsoleLocked" = Yes`: true,
		`{"CGSSessionScreenIsLocked"=Yes,"kCGSSessionOnConsoleKey"=Yes}`: true,
		`{"CGSSessionScreenIsLocked"=No}`:                                false,
	}
	for in, want := range cases {
		if ParseLocked(in) != want {
			t.Fatalf("ParseLocked(%q) != %v", in, want)
		}
	}
	if locked, err := Locked(context.Background()); err == nil && locked {
		t.Log("screen reported locked while tests run")
	}
}
