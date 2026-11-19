package server

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
)

type fakeStart struct {
	bin     string
	args    []string
	env     []string
	logPath string
	calls   int
	done    chan struct{}
}

func (f *fakeStart) start(bin string, args, env []string, logPath string) (<-chan struct{}, error) {
	f.bin, f.args, f.env, f.logPath = bin, args, env, logPath
	f.calls++
	f.done = make(chan struct{})
	return f.done, nil
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// migrateServer sets up a Vigil home with one task that mentions a packed state dir and a repo that
// is not packed.
func migrateServer(t *testing.T) (*Server, http.Handler, *fakeStart, string) {
	t.Helper()
	s, _ := newServer(t)
	home := t.TempDir()
	root := filepath.Join(home, ".vigil")
	writeFile(t, filepath.Join(root, "config.yaml"), "addr: 127.0.0.1:7788\n")
	writeFile(t, filepath.Join(root, "token"), "secret")
	writeFile(t, filepath.Join(root, "tasks", "watch", "brief.md"), "游标在 ~/.watcher/state，仓库 ~/code/demo-repo\n")
	writeFile(t, filepath.Join(home, ".watcher", "state", "cursor.json"), `{"ts":"1"}`)
	fs := &fakeStart{}
	s.Migrator = &Migrator{Home: home, Bin: "/opt/vigil/bin/vigil", Start: fs.start, Load: func() (config.Config, error) {
		return config.Config{Root: root, MigrateExtra: []string{"~/.watcher", "~/.gone"}}, nil
	}}
	s.Now = func() time.Time { return time.Date(2026, 3, 4, 5, 6, 0, 0, time.Local) }
	return s, s.Handler(), fs, home
}

func TestMigratePlanListsPackAndWhatTheNewMachineNeeds(t *testing.T) {
	_, h, _, _ := migrateServer(t)

	rec, env := do(h, "GET", "/api/migrate/plan", "", true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"archive":"~/vigil-export-20260304-0506.tar.gz"`,
		`"import_command":"bin/vigil import ~/vigil-export-20260304-0506.tar.gz"`,
		`"path":"~/.watcher","files":1`,
		`"extras_missing":["~/.gone"]`,
		`"needed":[{"path":"~/code/demo-repo","exists":false}]`,
		`"home":{"path":"~/.vigil","files":2`, // config.yaml and brief.md; the token stays behind
		`"busy":{"rounds":0,"jobs":0,"pending":0}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan lacks %s:\n%s", want, body)
		}
	}
	if !env.OK {
		t.Fatalf("env = %+v", env)
	}
}

func TestMigrateExportRefusesWithoutConfirm(t *testing.T) {
	_, h, fs, _ := migrateServer(t)

	for _, body := range []string{"", `{}`, `{"confirm":false}`} {
		if rec, _ := do(h, "POST", "/api/migrate/export", body, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: code %d", body, rec.Code)
		}
	}
	if fs.calls != 0 {
		t.Fatalf("export started %d times without confirm", fs.calls)
	}
}

func TestMigrateExportStartsTheCLIOnceAtATime(t *testing.T) {
	_, h, fs, home := migrateServer(t)
	pack := filepath.Join(home, "vigil-export-20260304-0506.tar.gz")

	rec, _ := do(h, "POST", "/api/migrate/export", `{"confirm":true}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if fs.bin != "/opt/vigil/bin/vigil" || !slices.Equal(fs.args, []string{"export", "-o", pack}) || fs.logPath != pack+".log" {
		t.Fatalf("started %s %v (log %s)", fs.bin, fs.args, fs.logPath)
	}
	if !slices.Contains(fs.env, "VIGIL_HOME="+filepath.Join(home, ".vigil")) {
		t.Fatal("export not pointed at this daemon's home")
	}
	for _, want := range []string{`"path":"` + pack + `"`, `"log":"` + pack + `.log"`, `"import_command":"bin/vigil import ~/vigil-export-20260304-0506.tar.gz"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("reply lacks %s: %s", want, rec.Body)
		}
	}

	if rec, _ := do(h, "POST", "/api/migrate/export", `{"confirm":true}`, true); rec.Code != http.StatusConflict {
		t.Fatalf("second start while the first runs: code %d", rec.Code)
	}
	if rec, _ := do(h, "GET", "/api/migrate/plan", "", true); !strings.Contains(rec.Body.String(), `"exporting":{"path":"`+pack) {
		t.Fatal("plan does not show the export in progress")
	}

	close(fs.done) // the first one ended (it failed and the daemon carried on, say)
	if rec, _ := do(h, "POST", "/api/migrate/export", `{"confirm":true}`, true); rec.Code != http.StatusOK || fs.calls != 2 {
		t.Fatalf("retry after the first ended: code %d, calls %d", rec.Code, fs.calls)
	}
}

func TestMigrateDoctorReturnsFindings(t *testing.T) {
	s, h, _, home := migrateServer(t)
	root := filepath.Join(home, ".vigil")
	s.Migrator.Load = func() (config.Config, error) {
		return config.Config{Root: root}, nil
	}

	rec, _ := do(h, "GET", "/api/migrate/doctor", "", true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	for _, want := range []string{`"kind":"path","subject":"~/code/demo-repo"`, `"failed":1`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("doctor lacks %s: %s", want, rec.Body)
		}
	}
}

func TestMigrateWithoutMigratorIsABadRequest(t *testing.T) {
	_, h := newServer(t)
	if rec, _ := do(h, "GET", "/api/migrate/plan", "", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d", rec.Code)
	}
}
