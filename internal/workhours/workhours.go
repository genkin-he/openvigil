// Package workhours says when it is polite to chase someone: weekdays, within the working hours of the
// day, in local time. Public holidays are not known.
package workhours

import (
	"fmt"
	"strings"
	"time"
)

// Hours is a daily window on Monday to Friday. A nil *Hours means any time.
type Hours struct {
	start, end time.Duration // since midnight
}

// Parse reads "HH:MM-HH:MM", for example "10:00-19:00".
func Parse(s string) (*Hours, error) {
	from, to, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return nil, fmt.Errorf("工作时间写成 10:00-19:00，不是 %q", s)
	}
	start, err1 := clock(from)
	end, err2 := clock(to)
	if err1 != nil || err2 != nil || end <= start {
		return nil, fmt.Errorf("工作时间写成 10:00-19:00（开始早于结束），不是 %q", s)
	}
	return &Hours{start: start, end: end}, nil
}

func clock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Contains reports whether t falls on a weekday inside the window.
func (h *Hours) Contains(t time.Time) bool {
	if h == nil {
		return true
	}
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	since := t.Sub(midnight(t))
	return since >= h.start && since < h.end
}

// Next is t itself when inside the window, otherwise the start of the next working window.
func (h *Hours) Next(t time.Time) time.Time {
	if h.Contains(t) {
		return t
	}
	day := midnight(t)
	if t.Sub(day) >= h.start { // past today's start (so after the end, or a weekend): from tomorrow
		day = day.AddDate(0, 0, 1)
	}
	for wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday; wd = day.Weekday() {
		day = day.AddDate(0, 0, 1)
	}
	return day.Add(h.start)
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
