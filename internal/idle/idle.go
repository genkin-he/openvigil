// Package idle reports how long the owner has not touched keyboard or mouse (macOS HID idle time).
package idle

import (
	"context"
	"errors"
	"fmt"
	osexec "os/exec"
	"regexp"
	"strconv"
	"time"
)

var hidIdle = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// Parse extracts the HID idle time from `ioreg -c IOHIDSystem` output.
func Parse(out string) (time.Duration, error) {
	m := hidIdle.FindStringSubmatch(out)
	if m == nil {
		return 0, errors.New("HIDIdleTime not found")
	}
	ns, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse HIDIdleTime: %w", err)
	}
	return time.Duration(ns), nil
}

// Duration returns the current idle time.
func Duration(ctx context.Context) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := osexec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, fmt.Errorf("ioreg: %w", err)
	}
	return Parse(string(out))
}

var lockedMarks = []*regexp.Regexp{
	regexp.MustCompile(`"CGSSessionScreenIsLocked"\s*=\s*Yes`),
	regexp.MustCompile(`"IOConsoleLocked"\s*=\s*Yes`),
}

// ParseLocked reports whether `ioreg -n Root -d1` output says the screen is locked.
func ParseLocked(out string) bool {
	for _, re := range lockedMarks {
		if re.MatchString(out) {
			return true
		}
	}
	return false
}

// Locked reports whether the screen is locked (GUI automation cannot work then).
func Locked(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := osexec.CommandContext(ctx, "ioreg", "-n", "Root", "-d1").Output()
	if err != nil {
		return false, fmt.Errorf("ioreg: %w", err)
	}
	return ParseLocked(string(out)), nil
}
