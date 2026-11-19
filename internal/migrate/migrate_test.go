package migrate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// oldMachine lays out a Vigil home on the machine being moved away from.
func oldMachine(t *testing.T) (home, root string) {
	home = t.TempDir()
	root = filepath.Join(home, ".vigil")
	write(t, filepath.Join(root, "config.yaml"), "source_dir: "+home+"/code/vigil\n")
	write(t, filepath.Join(root, "token"), "secret-token\n")
	write(t, filepath.Join(root, "vigil.log"), "log line\n")
	write(t, filepath.Join(root, "daemon.out"), "out\n")
	write(t, filepath.Join(root, MovedOutFile), "{}")
	write(t, filepath.Join(root, "tasks", "watch", "brief.md"), "状态在 ~/.watcher/state/cursor.json 和 "+home+"/code/vigil\n")
	write(t, filepath.Join(root, "tasks", "watch", "memory.md.bak-1"), "old lesson\n")
	write(t, filepath.Join(home, ".watcher", "state", "cursor.json"), `{"ts":"1"}`)
	return home, root
}

func TestExportThenImportOnAMachineWithAnotherHome(t *testing.T) {
	home, root := oldMachine(t)
	var buf bytes.Buffer
	m := Manifest{Home: home, Root: root, ExportedAt: time.Unix(1794884400, 0)}
	stats, err := Export(&buf, m, []string{filepath.Join(home, ".watcher", "state")})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files == 0 {
		t.Fatal("no files exported")
	}

	newHome := t.TempDir()
	newRoot := filepath.Join(newHome, ".vigil")
	got, err := Import(bytes.NewReader(buf.Bytes()), newRoot, newHome)
	if err != nil {
		t.Fatal(err)
	}
	if got.Home != home || !slices.Equal(got.Extras, []string{".watcher/state"}) {
		t.Fatalf("manifest = %+v", got)
	}
	if b := read(t, filepath.Join(newRoot, "tasks", "watch", "brief.md")); !strings.Contains(b, newHome+"/code/vigil") || strings.Contains(b, home) {
		t.Fatalf("brief not rewritten: %q", b)
	}
	if c := read(t, filepath.Join(newRoot, "config.yaml")); c != "source_dir: "+newHome+"/code/vigil\n" {
		t.Fatalf("config = %q", c)
	}
	if read(t, filepath.Join(newRoot, "tasks", "watch", "memory.md.bak-1")) != "old lesson\n" {
		t.Fatal("rule backups must travel: the evolve ledger reverts to them")
	}
	if read(t, filepath.Join(newHome, ".watcher", "state", "cursor.json")) != `{"ts":"1"}` {
		t.Fatal("extra state dir not restored")
	}
	for _, left := range []string{"token", "vigil.log", "daemon.out", MovedOutFile} {
		if _, err := os.Stat(filepath.Join(newRoot, left)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should stay behind, stat err = %v", left, err)
		}
	}
}

func TestExportRefusesExtrasOutsideHome(t *testing.T) {
	home, root := oldMachine(t)
	_, err := Export(&bytes.Buffer{}, Manifest{Home: home, Root: root}, []string{t.TempDir()})
	if err == nil {
		t.Fatal("an extra outside home cannot be put back on another machine; want an error")
	}
}

func TestImportRefusesAnExistingVigil(t *testing.T) {
	home, root := oldMachine(t)
	var buf bytes.Buffer
	if _, err := Export(&buf, Manifest{Home: home, Root: root}, nil); err != nil {
		t.Fatal(err)
	}
	newHome := t.TempDir()
	newRoot := filepath.Join(newHome, ".vigil")
	write(t, filepath.Join(newRoot, "config.yaml"), "addr: x\n")
	if _, err := Import(bytes.NewReader(buf.Bytes()), newRoot, newHome); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if read(t, filepath.Join(newRoot, "config.yaml")) != "addr: x\n" {
		t.Fatal("existing config touched")
	}
}

func TestImportRejectsEntriesThatEscape(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	add(manifestName, `{"version":1,"home":"/home/old"}`)
	add("home/../../evil", "x")
	_ = tw.Close()
	_ = gz.Close()

	newHome := t.TempDir()
	if _, err := Import(&buf, filepath.Join(newHome, ".vigil"), newHome); err == nil {
		t.Fatal("want an error for an entry leaving the target")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(newHome), "evil")); err == nil {
		t.Fatal("escaping entry was written")
	}
}

func TestNeededPathsGroupsReferencesOutsideVigil(t *testing.T) {
	home, root := oldMachine(t)
	write(t, filepath.Join(root, "tasks", "watch", "runbook.md"), "跑 ~/.local/bin/herdr，再看 ~/.vigil/inbox 和 ~/LB/repo/sub/dir。\n")
	got := NeededPaths(root, home)
	want := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".watcher", "state"),
		filepath.Join(home, "LB", "repo"),
		filepath.Join(home, "code", "vigil"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
