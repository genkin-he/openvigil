package patch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repo makes a git repository on main whose `make test` passes only while note.txt says "good".
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Makefile"), "test:\n\tgrep -q good note.txt\n")
	write(t, filepath.Join(dir, "note.txt"), "good\n")
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init")
	return dir
}

// diffFor edits note.txt in a throwaway clone and returns the git diff, the way an agent would.
func diffFor(t *testing.T, src, note string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "c")
	git(t, "", "clone", "-q", src, clone)
	write(t, filepath.Join(clone, "note.txt"), note)
	return git(t, clone, "diff")
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func propose(t *testing.T, diff string) (Store, Proposal) {
	t.Helper()
	st := Store{Root: t.TempDir()}
	p, err := st.Add(Proposal{Task: "self-evolve", Title: "改 note"}, diff, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	return st, p
}

func applier(dir string) Applier {
	return Applier{Repo: dir, Run: func(ctx context.Context, d, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}}
}

func TestAnApprovedDiffLandsOnMainAfterItsTestsPass(t *testing.T) {
	dir := repo(t)
	st, p := propose(t, diffFor(t, dir, "good, better\n"))
	sha, err := applier(dir).Land(context.Background(), st.DiffPath(p.ID), "改 note")
	if err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(git(t, dir, "rev-parse", "main")); head != sha {
		t.Fatalf("main = %s, want %s", head, sha)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "note.txt")); string(got) != "good, better\n" {
		t.Fatalf("note.txt = %q", got)
	}
	if wts := git(t, dir, "worktree", "list"); strings.Count(wts, "\n") != 1 {
		t.Fatalf("scratch worktree left behind:\n%s", wts)
	}
}

func TestADiffThatBreaksTheTestsLeavesMainAlone(t *testing.T) {
	dir := repo(t)
	before := git(t, dir, "rev-parse", "main")
	st, p := propose(t, diffFor(t, dir, "bad\n"))
	_, err := applier(dir).Land(context.Background(), st.DiffPath(p.ID), "改 note")
	if err == nil || !strings.Contains(err.Error(), "跑测试") {
		t.Fatalf("err = %v", err)
	}
	if after := git(t, dir, "rev-parse", "main"); after != before {
		t.Fatal("main moved although the tests failed")
	}
	if wts := git(t, dir, "worktree", "list"); strings.Count(wts, "\n") != 1 {
		t.Fatalf("scratch worktree left behind:\n%s", wts)
	}
}

func TestADiffThatNoLongerFitsIsRefused(t *testing.T) {
	dir := repo(t)
	diff := diffFor(t, dir, "good, better\n")
	write(t, filepath.Join(dir, "note.txt"), "good, changed meanwhile\n")
	git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-am", "meanwhile")
	st, p := propose(t, diff)
	if _, err := applier(dir).Land(context.Background(), st.DiffPath(p.ID), "改 note"); err == nil || !strings.Contains(err.Error(), "应用 diff") {
		t.Fatalf("err = %v", err)
	}
}

func TestNothingLandsWhileTheRepoIsOnAnotherBranch(t *testing.T) {
	dir := repo(t)
	git(t, dir, "switch", "-q", "-c", "work")
	st, p := propose(t, diffFor(t, dir, "good, better\n"))
	if _, err := applier(dir).Land(context.Background(), st.DiffPath(p.ID), "改 note"); err == nil || !strings.Contains(err.Error(), "不在 main") {
		t.Fatalf("err = %v", err)
	}
}

func TestAProposalNeedsAGitDiffOfSaneSize(t *testing.T) {
	st := Store{Root: t.TempDir()}
	ok := Proposal{Task: "self-evolve", Title: "x"}
	for name, c := range map[string]struct {
		p    Proposal
		diff string
	}{
		"no title":   {Proposal{Task: "self-evolve"}, "diff --git a/x b/x\n"},
		"empty":      {ok, "  "},
		"not a diff": {ok, "rm -rf /\n"},
		"too big":    {ok, "diff --git a/x b/x\n" + strings.Repeat("+x\n", MaxDiff/3+1)},
	} {
		if _, err := st.Add(c.p, c.diff, time.Now()); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	p, err := st.Add(ok, "diff --git a/x b/x\n", time.Unix(5, 0))
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(p.ID)
	if err != nil || got.Status != StatusProposed || got.Title != "x" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := st.Get("../../etc"); err == nil {
		t.Fatal("read outside the store")
	}
	if all, _ := st.List(); len(all) != 1 {
		t.Fatalf("list = %+v", all)
	}
}
