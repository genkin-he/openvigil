// Package probe looks up whether a thing a follow-up waits for (a GitLab merge request, or anything a
// lookup in config.yaml knows, such as a ticket) has been settled, read only, so the follow-up closes as
// soon as it is, not when it falls due.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Timeout bounds one lookup; a slow CLI must not hold up the other follow-ups.
const Timeout = 30 * time.Second

// Result says what a lookup found.
type Result struct {
	Known   bool   // the link or ref is something this package can look up
	Settled bool   // done one way or the other: nothing left to wait for
	Outcome string // one line for the follow-up's outcome when settled
}

// Custom is a lookup from config.yaml (probes:): a link or ref matching Match is handed to Command, a
// shell command run with the link and the ref as its two arguments. Its first output line is
// "settled <outcome>" when nothing is left to wait for, or "pending"; a non-zero exit is an error.
type Custom struct {
	Name    string `yaml:"name" json:"name"`
	Match   string `yaml:"match" json:"match"`
	Command string `yaml:"command" json:"command"`
}

type custom struct {
	Custom
	match *regexp.Regexp
}

// Checker runs the lookups. Run executes a command and returns its standard output.
type Checker struct {
	Run     func(ctx context.Context, name string, args ...string) ([]byte, error)
	customs []custom
}

// New builds a Checker with the configured lookups, refusing one without a name or command or with a
// pattern that does not compile.
func New(run func(ctx context.Context, name string, args ...string) ([]byte, error), customs []Custom) (Checker, error) {
	k := Checker{Run: run}
	for _, c := range customs {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Command) == "" {
			return Checker{}, fmt.Errorf("probe %q: name 和 command 都要填", c.Name)
		}
		re, err := regexp.Compile(c.Match)
		if err != nil {
			return Checker{}, fmt.Errorf("probe %s: match 不是合法的正则: %w", c.Name, err)
		}
		k.customs = append(k.customs, custom{Custom: c, match: re})
	}
	return k, nil
}

var mergeRequestURL = regexp.MustCompile(`^https://[^/]+/(.+?)/-/merge_requests/(\d+)`)

// Check looks up what link or ref points at. Unknown kinds come back with Known false and no error.
func (c Checker) Check(ctx context.Context, link, ref string) (Result, error) {
	if m := mergeRequestURL.FindStringSubmatch(link); m != nil {
		return c.mergeRequest(ctx, m[1], m[2])
	}
	for _, k := range c.customs {
		if (link != "" && k.match.MatchString(link)) || (ref != "" && k.match.MatchString(ref)) {
			return c.custom(ctx, k, link, ref)
		}
	}
	return Result{}, nil
}

func (c Checker) mergeRequest(ctx context.Context, repo, iid string) (Result, error) {
	var mr struct {
		State string `json:"state"`
	}
	if err := c.json(ctx, &mr, "glab", "mr", "view", iid, "-R", repo, "-F", "json"); err != nil {
		return Result{Known: true}, fmt.Errorf("read MR %s!%s: %w", repo, iid, err)
	}
	switch mr.State {
	case "merged":
		return Result{Known: true, Settled: true, Outcome: "MR 已合并"}, nil
	case "closed":
		return Result{Known: true, Settled: true, Outcome: "MR 已关闭（没合）"}, nil
	}
	return Result{Known: true}, nil
}

// custom runs a configured lookup. The link and ref go in as positional arguments, never into the
// command text, so nothing in them is interpreted by the shell.
func (c Checker) custom(ctx context.Context, k custom, link, ref string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := c.Run(ctx, "/bin/bash", "-c", k.Command+` "$1" "$2"`, "probe", link, ref)
	if err != nil {
		return Result{Known: true}, fmt.Errorf("probe %s: %w", k.Name, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	line = strings.TrimSpace(line)
	if line == "pending" {
		return Result{Known: true}, nil
	}
	if outcome, ok := strings.CutPrefix(line, "settled"); ok && (outcome == "" || outcome[0] == ' ') {
		return Result{Known: true, Settled: true, Outcome: firstNonEmpty(strings.TrimSpace(outcome), "已办完")}, nil
	}
	return Result{Known: true}, fmt.Errorf("probe %s: %w: %s", k.Name, errUnreadable, clip(line, 120))
}

var errUnreadable = errors.New(`output should start with "settled <outcome>" or "pending"`)

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (c Checker) json(ctx context.Context, v any, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := c.Run(ctx, name, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("parse output: %w", err)
	}
	return nil
}

// Command runs a real command and returns its standard output, with standard error in the error.
func Command(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return nil, fmt.Errorf("%s: %w: %s", name, err, clip(string(ee.Stderr), 300))
	}
	return out, err
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
