// Package patch keeps changes to Vigil's own code that an agent proposed, and lands one only after the
// owner approved it in the console. Agents never touch the repository: they hand in a diff, and this
// package (plain code, no agent) applies it in a scratch worktree, runs the tests, and fast-forwards the
// main branch; installing the new build is the caller's job.
package patch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Statuses a proposal moves through.
const (
	StatusProposed   = "proposed"
	StatusApplying   = "applying"   // approved; being applied and tested
	StatusInstalling = "installing" // merged; the new build is being installed
	StatusInstalled  = "installed"
	StatusFailed     = "failed"
	StatusDeclined   = "declined"
)

// MaxDiff bounds a proposal; a change bigger than this is not one to approve from a card.
const MaxDiff = 256 << 10

// Proposal is one change to Vigil's code waiting for, or past, the owner's decision.
type Proposal struct {
	ID      string    `json:"id"`
	Task    string    `json:"task"`
	Title   string    `json:"title"`
	Why     string    `json:"why"`
	Status  string    `json:"status"`
	Result  string    `json:"result,omitempty"` // what happened when it was applied
	Commit  string    `json:"commit,omitempty"`
	Created time.Time `json:"created_at"`
	Updated time.Time `json:"updated_at"`
}

// Store keeps each proposal in <Root>/<id>/ (meta.json, change.diff, and the install log once landed).
type Store struct{ Root string }

var idPattern = regexp.MustCompile(`^pt-[0-9a-f]{6}$`)

// Add saves a new proposal. The diff must be a git diff of reasonable size.
func (s Store) Add(p Proposal, diff string, now time.Time) (Proposal, error) {
	p.Title, p.Task = strings.TrimSpace(p.Title), strings.TrimSpace(p.Task)
	switch {
	case p.Title == "" || p.Task == "":
		return Proposal{}, errors.New("补丁需要 title 和 task")
	case strings.TrimSpace(diff) == "":
		return Proposal{}, errors.New("补丁的 diff 是空的")
	case len(diff) > MaxDiff:
		return Proposal{}, fmt.Errorf("diff 太大（%d 字节，上限 %d）：拆成几个小改动分别提", len(diff), MaxDiff)
	case !strings.Contains(diff, "diff --git "):
		return Proposal{}, errors.New("diff 要是 git diff 的输出（在仓库副本里改完跑 git diff）")
	}
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return Proposal{}, err
	}
	p.ID, p.Status, p.Result, p.Commit, p.Created, p.Updated = "pt-"+hex.EncodeToString(b), StatusProposed, "", "", now, now
	if err := os.MkdirAll(s.dir(p.ID), 0o700); err != nil {
		return Proposal{}, err
	}
	if err := os.WriteFile(s.DiffPath(p.ID), []byte(diff), 0o600); err != nil {
		return Proposal{}, err
	}
	return p, s.Save(p)
}

// Get reads a proposal.
func (s Store) Get(id string) (Proposal, error) {
	if !idPattern.MatchString(id) {
		return Proposal{}, fmt.Errorf("没有补丁 %s", id)
	}
	data, err := os.ReadFile(filepath.Join(s.dir(id), "meta.json"))
	if err != nil {
		return Proposal{}, fmt.Errorf("没有补丁 %s: %w", id, err)
	}
	var p Proposal
	return p, json.Unmarshal(data, &p)
}

// Save writes a proposal's metadata atomically.
func (s Store) Save(p Proposal) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir(p.ID), ".meta.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir(p.ID), "meta.json"))
}

// List returns every proposal, oldest first.
func (s Store) List() ([]Proposal, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Proposal
	for _, e := range entries {
		if p, err := s.Get(e.Name()); err == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// DiffPath is where a proposal's diff lives; InstallLog and InstallExit are written by the install.
func (s Store) DiffPath(id string) string    { return filepath.Join(s.dir(id), "change.diff") }
func (s Store) InstallLog(id string) string  { return filepath.Join(s.dir(id), "install.log") }
func (s Store) InstallExit(id string) string { return filepath.Join(s.dir(id), "install.exit") }

func (s Store) dir(id string) string { return filepath.Join(s.Root, id) }

// Run executes a command in dir and returns its combined output.
type Run func(ctx context.Context, dir, name string, args ...string) (string, error)

// Command is the real Run.
func Command(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := osexec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Applier lands approved diffs on Branch of the repository at Repo.
type Applier struct {
	Repo    string
	Branch  string // defaults to main
	Run     Run
	Scratch string // where worktrees go; defaults to the system temp dir
}

// Land applies the diff in a scratch worktree off Branch, runs `make test` there, commits with message,
// and fast-forwards Branch to that commit. Nothing changes on Branch unless every step passed. It
// returns the new commit.
func (a Applier) Land(ctx context.Context, diffPath, message string) (string, error) {
	branch := a.Branch
	if branch == "" {
		branch = "main"
	}
	run := a.Run
	if run == nil {
		run = Command
	}
	if cur, err := run(ctx, a.Repo, "git", "symbolic-ref", "--short", "HEAD"); err != nil || strings.TrimSpace(cur) != branch {
		return "", fmt.Errorf("仓库 %s 当前不在 %s 分支上（%s），先切回去再应用", a.Repo, branch, strings.TrimSpace(cur))
	}
	wt, err := os.MkdirTemp(a.Scratch, "vigil-patch-")
	if err != nil {
		return "", err
	}
	if err := os.Remove(wt); err != nil { // git worktree add wants to create it
		return "", err
	}
	if out, err := run(ctx, a.Repo, "git", "worktree", "add", "--detach", wt, branch); err != nil {
		return "", fmt.Errorf("建工作区：%w：%s", err, clip(out))
	}
	defer func() {
		if out, err := run(context.Background(), a.Repo, "git", "worktree", "remove", "--force", wt); err != nil {
			_ = os.RemoveAll(wt)
			_, _ = run(context.Background(), a.Repo, "git", "worktree", "prune")
			_ = out
		}
	}()
	steps := []struct {
		what string
		name string
		args []string
	}{
		{"应用 diff（和现在的代码对不上的话，要让它基于最新代码重新出）", "git", []string{"apply", "--index", diffPath}},
		{"跑测试（make test）", "make", []string{"test"}},
		{"提交", "git", []string{"-c", "commit.gpgsign=false", "commit", "-q", "-m", message}},
	}
	for _, st := range steps {
		if out, err := run(ctx, wt, st.name, st.args...); err != nil {
			return "", fmt.Errorf("%s失败：%w\n%s", st.what, err, clip(out))
		}
	}
	sha, err := run(ctx, wt, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("读提交：%w", err)
	}
	sha = strings.TrimSpace(sha)
	if out, err := run(ctx, a.Repo, "git", "merge", "--ff-only", "-q", sha); err != nil {
		return "", fmt.Errorf("合进 %s 失败（%s 在这期间有新提交，或工作区里有冲突的未提交改动）：%w\n%s", branch, branch, err, clip(out))
	}
	return sha, nil
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	const max = 1500
	if len(s) > max {
		return "…" + s[len(s)-max:]
	}
	return s
}
