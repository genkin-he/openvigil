// Package task defines a Vigil task: what triggers it, which agent runs it and how a run behaves.
package task

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseCron parses a standard 5-field cron expression or a descriptor such as @hourly.
func ParseCron(expr string) (cron.Schedule, error) {
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", expr, err)
	}
	return sched, nil
}

// Trigger kinds.
const (
	TriggerOnce   = "once"
	TriggerCron   = "cron"
	TriggerEvery  = "every"
	TriggerStream = "stream"
	TriggerManual = "manual"
)

// Execution modes.
const (
	ModeHeadless = "headless"
	ModeResident = "resident"
)

// Session modes.
const (
	SessionFresh    = "fresh"
	SessionContinue = "continue"
)

// Catch-up policies for runs missed while the machine slept.
const (
	CatchUpSkip = "skip"
	CatchUpOnce = "once"
)

// Evolve policies for runbook amendments.
const (
	EvolveOff     = "off"
	EvolvePropose = "propose"
	EvolveAuto    = "auto"
)

const (
	defaultAgentKind   = "claude"
	defaultPermission  = "auto"
	defaultTimeout     = 20 * time.Minute
	defaultRotateEvery = 8
	defaultBatchWindow = 20 * time.Second
	defaultMaxBatch    = 50
	defaultRetryMax    = 1
	defaultRetryWait   = 2 * time.Minute
	minEvery           = time.Minute
)

var (
	idPattern        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	agentKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// Duration is a time.Duration that reads and writes as "20m" in YAML/JSON.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

func (d *Duration) UnmarshalJSON(b []byte) error {
	parsed, err := time.ParseDuration(strings.Trim(string(b), `"`))
	if err != nil {
		return fmt.Errorf("invalid duration %s: %w", b, err)
	}
	*d = Duration(parsed)
	return nil
}

// Producer is a long-running process a stream depends on (e.g. the script that writes the log the
// stream command follows). Vigil keeps it alive while the task is enabled and stops it when paused.
type Producer struct {
	Command string `yaml:"command" json:"command"`
	PIDFile string `yaml:"pidfile" json:"pidfile"`                 // where the producer pid is kept, so a running one is adopted
	Match   string `yaml:"match,omitempty" json:"match,omitempty"` // substring the pid's command line must contain (guards pid reuse)
}

type Stream struct {
	Command     string    `yaml:"command" json:"command"`
	Producer    *Producer `yaml:"producer,omitempty" json:"producer,omitempty"`
	Ignore      []string  `yaml:"ignore,omitempty" json:"ignore,omitempty"` // regexps; matching lines are dropped before spooling
	BatchWindow Duration  `yaml:"batch_window,omitempty" json:"batch_window,omitempty"`
	MaxBatch    int       `yaml:"max_batch,omitempty" json:"max_batch,omitempty"`
	Light       *Light    `yaml:"light,omitempty" json:"light,omitempty"`
}

// Light sends easy batches to a cheaper model: when every event of a batch matches one of Match, the
// round runs headless and fresh with Model, leaving the task's own session alone.
type Light struct {
	Model string   `yaml:"model" json:"model"`
	Match []string `yaml:"match" json:"match"` // regexps over event lines
}

type Trigger struct {
	Kind   string    `yaml:"kind" json:"kind"`
	Cron   string    `yaml:"cron,omitempty" json:"cron,omitempty"`
	Every  Duration  `yaml:"every,omitempty" json:"every,omitempty"`
	At     time.Time `yaml:"at,omitempty" json:"at,omitzero"`
	Stream *Stream   `yaml:"stream,omitempty" json:"stream,omitempty"`
}

type Agent struct {
	Kind            string   `yaml:"kind" json:"kind"`
	Args            []string `yaml:"args,omitempty" json:"args,omitempty"`
	Cwd             string   `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Model           string   `yaml:"model,omitempty" json:"model,omitempty"`
	PermissionMode  string   `yaml:"permission_mode,omitempty" json:"permission_mode,omitempty"`
	DisallowedTools []string `yaml:"disallowed_tools,omitempty" json:"disallowed_tools,omitempty"`
	AllowedTools    []string `yaml:"allowed_tools,omitempty" json:"allowed_tools,omitempty"` // permanently approved operations, e.g. "Bash(touch:*)"
}

type Session struct {
	Mode        string `yaml:"mode" json:"mode"`
	RotateEvery int    `yaml:"rotate_every,omitempty" json:"rotate_every,omitempty"`
}

type Retry struct {
	Max     int      `yaml:"max" json:"max"`
	Backoff Duration `yaml:"backoff" json:"backoff"`
}

type Run struct {
	Timeout Duration `yaml:"timeout" json:"timeout"`
	CatchUp string   `yaml:"catch_up" json:"catch_up"`
	Retry   Retry    `yaml:"retry" json:"retry"`
}

type Notify struct {
	On      []string `yaml:"on,omitempty" json:"on,omitempty"`
	Command string   `yaml:"command,omitempty" json:"command,omitempty"`
}

// Hands lets a task operate the GUI (apps, browser). Such runs wait until the owner is away.
type Hands struct {
	GUI         bool     `yaml:"gui" json:"gui"`
	RequireIdle Duration `yaml:"require_idle,omitempty" json:"require_idle,omitempty"`
}

const defaultRequireIdle = 5 * time.Minute

// Task is the machine-readable part of a task, stored as task.yaml.
type Task struct {
	ID      string  `yaml:"id" json:"id"`
	Name    string  `yaml:"name" json:"name"`
	Enabled bool    `yaml:"enabled" json:"enabled"`
	Mode    string  `yaml:"mode" json:"mode"`
	Trigger Trigger `yaml:"trigger" json:"trigger"`
	Signal  string  `yaml:"signal,omitempty" json:"signal,omitempty"`
	Agent   Agent   `yaml:"agent" json:"agent"`
	Session Session `yaml:"session" json:"session"`
	Run     Run     `yaml:"run" json:"run"`
	Evolve  string  `yaml:"evolve" json:"evolve"`
	Notify  Notify  `yaml:"notify" json:"notify"`
	Hands   Hands   `yaml:"hands,omitempty" json:"hands,omitzero"`
}

// ValidID reports whether id is safe to use as a directory name and an agent name suffix.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Parse decodes YAML, fills defaults and validates.
func Parse(data []byte) (Task, error) {
	var t Task
	if err := yaml.Unmarshal(data, &t); err != nil {
		return Task{}, fmt.Errorf("parse task: %w", err)
	}
	t = WithDefaults(t)
	if err := Validate(t); err != nil {
		return Task{}, err
	}
	return t, nil
}

// Marshal encodes a task as YAML.
func Marshal(t Task) ([]byte, error) { return yaml.Marshal(t) }

// WithDefaults returns a copy of t with empty fields set to their defaults.
func WithDefaults(t Task) Task {
	out := t
	if out.Name == "" {
		out.Name = out.ID
	}
	if out.Mode == "" {
		out.Mode = ModeHeadless
	}
	if out.Agent.Kind == "" {
		out.Agent.Kind = defaultAgentKind
	}
	if out.Agent.PermissionMode == "" {
		out.Agent.PermissionMode = defaultPermission
	}
	out.Agent.Args = append([]string(nil), t.Agent.Args...)
	out.Agent.DisallowedTools = append([]string(nil), t.Agent.DisallowedTools...)
	out.Agent.AllowedTools = append([]string(nil), t.Agent.AllowedTools...)
	if out.Session.Mode == "" {
		out.Session.Mode = SessionContinue
	}
	if out.Session.RotateEvery == 0 {
		out.Session.RotateEvery = defaultRotateEvery
	}
	if out.Run.Timeout == 0 {
		out.Run.Timeout = Duration(defaultTimeout)
	}
	if out.Run.CatchUp == "" {
		out.Run.CatchUp = CatchUpOnce
	}
	if out.Run.Retry.Backoff == 0 {
		out.Run.Retry.Backoff = Duration(defaultRetryWait)
	}
	if out.Run.Retry.Max == 0 {
		out.Run.Retry.Max = defaultRetryMax
	}
	if out.Evolve == "" {
		out.Evolve = EvolvePropose
	}
	if out.Hands.GUI && out.Hands.RequireIdle == 0 {
		out.Hands.RequireIdle = Duration(defaultRequireIdle)
	}
	if len(out.Notify.On) == 0 {
		out.Notify.On = []string{"fail", "needs-you"}
	} else {
		out.Notify.On = append([]string(nil), t.Notify.On...)
	}
	if t.Trigger.Stream != nil {
		s := *t.Trigger.Stream
		s.Ignore = append([]string(nil), t.Trigger.Stream.Ignore...)
		if l := t.Trigger.Stream.Light; l != nil {
			cp := Light{Model: l.Model, Match: append([]string(nil), l.Match...)}
			s.Light = &cp
		}
		if p := t.Trigger.Stream.Producer; p != nil {
			cp := *p
			s.Producer = &cp
		}
		if s.BatchWindow == 0 {
			s.BatchWindow = Duration(defaultBatchWindow)
		}
		if s.MaxBatch == 0 {
			s.MaxBatch = defaultMaxBatch
		}
		out.Trigger.Stream = &s
	}
	return out
}

// Validate checks a task after defaults were applied.
func Validate(t Task) error {
	var errs []error
	if !ValidID(t.ID) {
		errs = append(errs, fmt.Errorf("id %q must match %s", t.ID, idPattern))
	}
	if t.Mode != ModeHeadless && t.Mode != ModeResident {
		errs = append(errs, fmt.Errorf("mode must be headless or resident, got %q", t.Mode))
	}
	if t.Mode == ModeHeadless && t.Agent.Kind != defaultAgentKind {
		errs = append(errs, fmt.Errorf("headless mode only supports agent.kind claude, got %q", t.Agent.Kind))
	}
	errs = append(errs, validateTrigger(t.Trigger)...)
	if !agentKindPattern.MatchString(t.Agent.Kind) {
		errs = append(errs, fmt.Errorf("agent.kind %q is invalid", t.Agent.Kind))
	}
	if t.Session.Mode != SessionFresh && t.Session.Mode != SessionContinue {
		errs = append(errs, fmt.Errorf("session.mode must be fresh or continue, got %q", t.Session.Mode))
	}
	if t.Session.RotateEvery < 1 {
		errs = append(errs, errors.New("session.rotate_every must be at least 1"))
	}
	if t.Run.Timeout.Std() < time.Minute {
		errs = append(errs, errors.New("run.timeout must be at least 1m"))
	}
	if t.Run.CatchUp != CatchUpSkip && t.Run.CatchUp != CatchUpOnce {
		errs = append(errs, fmt.Errorf("run.catch_up must be skip or once, got %q", t.Run.CatchUp))
	}
	if t.Run.Retry.Max < 0 || t.Run.Retry.Max > 5 {
		errs = append(errs, errors.New("run.retry.max must be between 0 and 5"))
	}
	switch t.Evolve {
	case EvolveOff, EvolvePropose, EvolveAuto:
	default:
		errs = append(errs, fmt.Errorf("evolve must be off, propose or auto, got %q", t.Evolve))
	}
	return errors.Join(errs...)
}

func validateTrigger(tr Trigger) []error {
	switch tr.Kind {
	case TriggerCron:
		if strings.TrimSpace(tr.Cron) == "" {
			return []error{errors.New("trigger.cron is required for cron triggers")}
		}
		if _, err := ParseCron(tr.Cron); err != nil {
			return []error{err}
		}
	case TriggerEvery:
		if tr.Every.Std() < minEvery {
			return []error{errors.New("trigger.every must be at least 1m")}
		}
	case TriggerOnce:
		if tr.At.IsZero() {
			return []error{errors.New("trigger.at is required for once triggers")}
		}
	case TriggerStream:
		if tr.Stream == nil || strings.TrimSpace(tr.Stream.Command) == "" {
			return []error{errors.New("trigger.stream.command is required for stream triggers")}
		}
		if _, err := IgnoreMatcher(tr.Stream.Ignore); err != nil {
			return []error{err}
		}
		if l := tr.Stream.Light; l != nil {
			if strings.TrimSpace(l.Model) == "" || len(l.Match) == 0 {
				return []error{errors.New("trigger.stream.light needs model and match")}
			}
			if _, err := IgnoreMatcher(l.Match); err != nil {
				return []error{fmt.Errorf("trigger.stream.light: %w", err)}
			}
		}
		if p := tr.Stream.Producer; p != nil && (strings.TrimSpace(p.Command) == "" || strings.TrimSpace(p.PIDFile) == "") {
			return []error{errors.New("trigger.stream.producer needs command and pidfile")}
		}
	case TriggerManual:
	default:
		return []error{fmt.Errorf("trigger.kind must be once, cron, every, stream or manual, got %q", tr.Kind)}
	}
	return nil
}

// IgnoreMatcher compiles stream ignore patterns into one predicate.
func IgnoreMatcher(patterns []string) (func(line string) bool, error) {
	res := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("trigger.stream.ignore %q: %w", p, err)
		}
		res = append(res, re)
	}
	return func(line string) bool {
		for _, re := range res {
			if re.MatchString(line) {
				return true
			}
		}
		return false
	}, nil
}

// AgentName is the herdr agent name that runs this task.
func AgentName(id string) string { return "v-" + id }
