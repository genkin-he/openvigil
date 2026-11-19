package migrate

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/health"
)

func findingFor(t *testing.T, fs []Finding, kind, subject string) Finding {
	t.Helper()
	for _, f := range fs {
		if f.Kind == kind && f.Subject == subject {
			return f
		}
	}
	t.Fatalf("no %s finding for %q in %+v", kind, subject, fs)
	return Finding{}
}

func TestDoctorReportsChecksSourceAndPaths(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".vigil")
	write(t, filepath.Join(root, "config.yaml"), "x: 1\n")
	write(t, filepath.Join(root, "tasks", "watch", "brief.md"), "游标在 ~/.watcher/state/cursor.json，仓库在 ~/code/missing-repo\n")
	write(t, filepath.Join(home, ".watcher", "state", "cursor.json"), "{}")
	cfg := config.Config{Root: root, SourceDir: filepath.Join(home, "code", "vigil"), Checks: []health.Check{
		{Name: "示例登录", Command: "true", Hint: "跑 demo login"},
		{Name: "示例工具", Command: "echo first; echo 没登录; exit 3", Hint: "跑 demo-tool auth"},
	}}

	got := Doctor(context.Background(), cfg, home)

	if ok := findingFor(t, got, KindCheck, "示例登录"); !ok.OK || ok.Hint != "" || ok.Detail != "" {
		t.Fatalf("passing check = %+v", ok)
	}
	bad := findingFor(t, got, KindCheck, "示例工具")
	if bad.OK || !strings.Contains(bad.Detail, "没登录") || bad.Hint != "跑 demo-tool auth" {
		t.Fatalf("failing check = %+v", bad)
	}
	if src := findingFor(t, got, KindSource, "~/code/vigil"); src.OK || src.Hint == "" {
		t.Fatalf("missing source repo = %+v", src)
	}
	if p := findingFor(t, got, KindPath, "~/.watcher/state"); !p.OK {
		t.Fatalf("existing path = %+v", p)
	}
	if p := findingFor(t, got, KindPath, "~/code/missing-repo"); p.OK || p.Hint == "" {
		t.Fatalf("missing path = %+v", p)
	}
	if n := Failed(got); n != 3 {
		t.Fatalf("failed = %d, want 3 (tool, source, repo)", n)
	}
	if slices.ContainsFunc(got, func(f Finding) bool { return f.Kind == KindMovedOut }) {
		t.Fatal("a home never exported reported as moved out")
	}
}

func TestDoctorFlagsAMovedOutHome(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".vigil")
	write(t, filepath.Join(root, "config.yaml"), "x: 1\n")
	if err := MarkMovedOut(root, "/tmp/pack.tar.gz", time.Now()); err != nil {
		t.Fatal(err)
	}

	got := Doctor(context.Background(), config.Config{Root: root}, home)

	if len(got) != 1 || got[0].Kind != KindMovedOut || got[0].OK || !strings.Contains(got[0].Detail, "/tmp/pack.tar.gz") {
		t.Fatalf("findings = %+v", got)
	}
	m, err := ReadMovedOut(root)
	if err != nil || m == nil || m.Archive != "/tmp/pack.tar.gz" {
		t.Fatalf("marker = %+v, %v", m, err)
	}
}

func TestRefuseMovedOutLetsAFreshHomeStart(t *testing.T) {
	if err := RefuseMovedOut(t.TempDir()); err != nil {
		t.Fatalf("fresh home refused: %v", err)
	}
}

func TestNotPackedSkipsPathsTheExtrasCover(t *testing.T) {
	h := "/h"
	need := []string{filepath.Join(h, ".tidy", "watch"), filepath.Join(h, "LB", "repo")}
	got := NotPacked(need, []string{filepath.Join(h, ".tidy", "watch")})
	if !slices.Equal(got, []string{filepath.Join(h, "LB", "repo")}) {
		t.Fatalf("got %v", got)
	}
}

func TestMeasureLeavesOutWhatStaysBehind(t *testing.T) {
	home, root := oldMachine(t)

	st, err := Measure(root, []string{filepath.Join(home, ".watcher")})

	if err != nil {
		t.Fatal(err)
	}
	// config.yaml, brief.md, memory.md.bak-1 under the home; cursor.json in the extra.
	if st.Files != 4 {
		t.Fatalf("files = %d, want 4", st.Files)
	}
}

func TestExpandExtrasSplitsPresentAndMissing(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".watcher", "a"), "1")

	present, missing, err := ExpandExtras([]string{"~/.watcher", "~/.gone"}, home)

	if err != nil || !slices.Equal(present, []string{filepath.Join(home, ".watcher")}) || !slices.Equal(missing, []string{filepath.Join(home, ".gone")}) {
		t.Fatalf("present=%v missing=%v err=%v", present, missing, err)
	}
	if _, _, err := ExpandExtras([]string{"relative/dir"}, home); err == nil {
		t.Fatal("relative path accepted")
	}
}
