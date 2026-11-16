package workhours

import (
	"testing"
	"time"
)

var sh = time.FixedZone("CST", 8*3600)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, sh)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextMovesOffHoursToTheNextWorkingMorning(t *testing.T) {
	h, err := Parse("10:00-19:00")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-13 is a Friday.
	for in, want := range map[string]string{
		"2026-11-13 15:00": "2026-11-13 15:00", // inside: unchanged
		"2026-11-13 08:30": "2026-11-13 10:00", // before work
		"2026-11-13 19:00": "2026-11-16 10:00", // Friday evening → Monday
		"2026-11-13 21:30": "2026-11-16 10:00",
		"2026-11-14 11:00": "2026-11-16 10:00", // Saturday
		"2026-11-15 23:59": "2026-11-16 10:00", // Sunday night
		"2026-11-16 18:59": "2026-11-16 18:59",
		"2026-11-17 20:00": "2026-11-18 10:00", // a weekday evening → next morning
	} {
		if got := h.Next(at(in)); !got.Equal(at(want)) {
			t.Errorf("Next(%s) = %s, want %s", in, got.Format("2006-01-02 15:04 Mon"), want)
		}
		if h.Contains(at(in)) != (in == want) {
			t.Errorf("Contains(%s) wrong", in)
		}
	}
}

func TestNilHoursMeanAnyTime(t *testing.T) {
	var h *Hours
	now := at("2026-11-14 03:00")
	if !h.Contains(now) || !h.Next(now).Equal(now) {
		t.Fatal("nil hours should not move anything")
	}
}

func TestBadHoursAreRejected(t *testing.T) {
	for _, s := range []string{"", "10-19", "19:00-10:00", "25:00-26:00", "10:00-10:00", "10:60-19:00"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
