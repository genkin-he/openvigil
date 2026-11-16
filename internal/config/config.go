// Package config loads Vigil's machine-level settings from <root>/config.yaml.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vigil/internal/health"
	"vigil/internal/probe"
)

const (
	defaultAddr  = "127.0.0.1:7788"
	defaultLabel = "dev.vigil.daemon"
	// DefaultEvolveTask is the self-evolution task every install starts with.
	DefaultEvolveTask  = "self-evolve"
	defaultSession     = "vigil"
	defaultUserSession = "default"
	defaultConcurrent  = 3
	defaultCheckEvery  = time.Hour
	tokenFile          = "token"
	configFile         = "config.yaml"
)

// Config holds machine-level settings.
type Config struct {
	Addr         string `yaml:"addr"`
	ClaudeBin    string `yaml:"claude_bin"`
	HerdrBin     string `yaml:"herdr_bin"`
	HerdrSession string `yaml:"herdr_session"` // where resident tasks live
	UserSession  string `yaml:"user_session"`  // where forge and takeover sessions open
	TerminalApp  string `yaml:"terminal_app"`  // app raised on takeover; empty = detect from the herdr client
	// Webhook, when set, receives needs-you / failure notices as POST {"text": …} (wechat_webhook is the old name).
	Webhook       string `yaml:"webhook,omitempty"`
	WechatWebhook string `yaml:"wechat_webhook,omitempty"`
	// LaunchdLabel names the launchd service (default dev.vigil.daemon).
	LaunchdLabel  string `yaml:"launchd_label,omitempty"`
	MaxConcurrent int    `yaml:"max_concurrent"`
	// Checks are the logins verified every CheckEvery; a failure becomes a to-do.
	Checks     []health.Check `yaml:"checks,omitempty"`
	CheckEvery string         `yaml:"check_every,omitempty"`
	// Probes are extra lookups for follow-ups (a ticket system, say): a link or ref matching one is
	// checked with its command, and the follow-up closes as soon as it reports settled.
	Probes []probe.Custom `yaml:"probes,omitempty"`
	// SourceDir is Vigil's own source repository. Set, agents may propose changes to it (vigil patch
	// propose); each lands only after the owner approves it in the console.
	SourceDir string `yaml:"source_dir,omitempty"`
	// FollowHours limits chasing people to weekdays inside these hours ("10:00-19:00"); a follow-up due
	// outside them waits for the next working morning. Empty: any time.
	FollowHours string `yaml:"follow_hours,omitempty"`
	// Slack names the Slack watch task and the owner's self-DM, used by the end-to-end self-test and
	// for answering to-dos from Slack; left empty, neither is on.
	Slack OwnerSlack `yaml:"slack,omitempty"`
	// EvolveTask is the self-evolution task, woken soon after the owner's feedback (corrections, edited or
	// overruled suggestions) on top of its own schedule (default self-evolve; "-" turns the wake-ups off).
	EvolveTask string `yaml:"evolve_task,omitempty"`
	// ReplayModel is the model that replays past rounds under changed rules (default sonnet).
	ReplayModel string `yaml:"replay_model,omitempty"`

	Root  string `yaml:"-"`
	Token string `yaml:"-"`
}

// Root returns $VIGIL_HOME or ~/.vigil.
func Root() (string, error) {
	if v := os.Getenv("VIGIL_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".vigil"), nil
}

// Load reads config.yaml (creating defaults for missing values) and the API token (creating one if needed).
func Load() (Config, error) {
	root, err := Root()
	if err != nil {
		return Config{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Config{}, fmt.Errorf("create %s: %w", root, err)
	}
	var c Config
	data, err := os.ReadFile(filepath.Join(root, configFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if err := yaml.Unmarshal(data, &c); err != nil {
			return Config{}, fmt.Errorf("parse config: %w", err)
		}
	}
	c = withDefaults(c)
	c.Root = root
	c.Token, err = loadToken(root)
	return c, err
}

// Save writes config.yaml.
func Save(c Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return os.WriteFile(filepath.Join(c.Root, configFile), data, 0o600)
}

func withDefaults(c Config) Config {
	out := c
	if out.Addr == "" {
		out.Addr = defaultAddr
	}
	if out.ReplayModel == "" {
		out.ReplayModel = "sonnet"
	}
	if out.ClaudeBin == "" {
		out.ClaudeBin = lookPath("claude")
	}
	if out.HerdrBin == "" {
		out.HerdrBin = lookPath("herdr")
	}
	if out.HerdrSession == "" {
		out.HerdrSession = defaultSession
	}
	if out.UserSession == "" {
		out.UserSession = defaultUserSession
	}
	if out.Webhook == "" {
		out.Webhook = out.WechatWebhook
	}
	if out.LaunchdLabel == "" {
		out.LaunchdLabel = defaultLabel
	}
	if out.EvolveTask == "" {
		out.EvolveTask = DefaultEvolveTask
	}
	if out.MaxConcurrent <= 0 {
		out.MaxConcurrent = defaultConcurrent
	}
	if len(out.Checks) == 0 {
		out.Checks = health.Defaults(out.Slack.Enabled())
	}
	if d, err := time.ParseDuration(out.CheckEvery); err != nil || d < time.Minute {
		out.CheckEvery = defaultCheckEvery.String()
	}
	return out
}

func lookPath(name string) string {
	if p, err := osexec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	// Tools installed per user (herdr, claude) often live in ~/.local/bin, which a shell may not have on PATH.
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".local", "bin", name); isExecutable(p) {
			return p
		}
	}
	return name
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

func loadToken(root string) (string, error) {
	path := filepath.Join(root, tokenFile)
	data, err := os.ReadFile(path)
	if err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return strings.TrimSpace(string(data)), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read token: %w", err)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tok := hex.EncodeToString(b)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return tok, nil
}

// URL is the console base URL.
func (c Config) URL() string { return "http://" + c.Addr }

// CheckInterval is how often the logins are checked.
func (c Config) CheckInterval() time.Duration {
	if d, err := time.ParseDuration(c.CheckEvery); err == nil && d >= time.Minute {
		return d
	}
	return defaultCheckEvery
}

// OwnerSlack is the owner's Slack setup as Vigil uses it.
type OwnerSlack struct {
	Task          string `yaml:"task,omitempty"`           // the stream task watching Slack
	SelfDM        string `yaml:"self_dm,omitempty"`        // the owner's DM with themselves
	SelfTestEvery string `yaml:"selftest_every,omitempty"` // automatic self-tests after an install are skipped within this long of a pass
	TodoMirror    *bool  `yaml:"todo_mirror,omitempty"`    // post to-dos to the self-DM and take answers from their threads (default on)
}

const defaultSelfTestEvery = 6 * time.Hour

// Enabled reports whether the Slack task and self-DM are configured.
func (s OwnerSlack) Enabled() bool { return s.Task != "" && s.SelfDM != "" }

// Mirror reports whether to-dos go to the self-DM too.
func (s OwnerSlack) Mirror() bool { return s.Enabled() && (s.TodoMirror == nil || *s.TodoMirror) }

// SelfTestInterval is the least time between automatic self-tests.
func (s OwnerSlack) SelfTestInterval() time.Duration {
	if d, err := time.ParseDuration(s.SelfTestEvery); err == nil && d > 0 {
		return d
	}
	return defaultSelfTestEvery
}
