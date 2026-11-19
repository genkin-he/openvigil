package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"vigil/internal/config"
	"vigil/internal/migrate"
)

// Migrator lets the console run the health check and start a move to another computer. The move itself
// is the CLI's `vigil export`, started detached in its own session: it stops this very daemon, so it must
// outlive it.
type Migrator struct {
	Home string
	Bin  string // the vigil executable
	// Load reads config.yaml afresh, so edits since the daemon started show up.
	Load func() (config.Config, error)
	// Start runs bin with args detached (own session, output to logPath); done closes when it exits.
	Start func(bin string, args, env []string, logPath string) (done <-chan struct{}, err error)

	mu      sync.Mutex
	running <-chan struct{}
	last    *exportStarted
}

type exportStarted struct {
	Path          string    `json:"path"`
	Log           string    `json:"log"`
	ImportCommand string    `json:"import_command"`
	StartedAt     time.Time `json:"started_at"`
}

type migrateBusy struct {
	Rounds  int `json:"rounds"`
	Jobs    int `json:"jobs"`
	Pending int `json:"pending"`
}

type packedDir struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

type neededPath struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"` // on this machine; the new one needs it too
}

type migratePlan struct {
	Archive       string         `json:"archive"`
	Log           string         `json:"log"`
	ImportCommand string         `json:"import_command"`
	Home          packedDir      `json:"home"`
	Extras        []packedDir    `json:"extras"`
	ExtrasMissing []string       `json:"extras_missing"`
	Needed        []neededPath   `json:"needed"`
	StayBehind    []string       `json:"stay_behind"`
	Total         migrate.Stats  `json:"total"`
	Busy          migrateBusy    `json:"busy"`
	Exporting     *exportStarted `json:"exporting,omitempty"`
}

const exportLogSuffix = ".log"

func (s *Server) migrator() (*Migrator, error) {
	if s.Migrator == nil || s.Migrator.Load == nil {
		return nil, badRequest("这个守护进程没有开启搬家功能")
	}
	return s.Migrator, nil
}

// migrateDoctor runs the same health check as `vigil doctor`.
func (s *Server) migrateDoctor(_ http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.migrator()
	if err != nil {
		return nil, err
	}
	cfg, err := m.Load()
	if err != nil {
		return nil, fmt.Errorf("读配置：%w", err)
	}
	findings := migrate.Doctor(r.Context(), cfg, m.Home)
	return map[string]any{"findings": findings, "failed": migrate.Failed(findings), "checked_at": s.Now()}, nil
}

// migratePlan tells what an export would pack and what the new machine has to provide itself.
func (s *Server) migratePlan(_ http.ResponseWriter, _ *http.Request) (any, error) {
	m, err := s.migrator()
	if err != nil {
		return nil, err
	}
	cfg, err := m.Load()
	if err != nil {
		return nil, fmt.Errorf("读配置：%w", err)
	}
	extras, missing, err := migrate.ExpandExtras(cfg.MigrateExtra, m.Home)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	homeSt, err := migrate.Measure(cfg.Root, nil)
	if err != nil {
		return nil, err
	}
	archive := archivePath(m.Home, s.Now())
	plan := migratePlan{
		Archive: migrate.Tilde(archive, m.Home), Log: migrate.Tilde(archive+exportLogSuffix, m.Home),
		ImportCommand: importCommand(archive, m.Home),
		Home:          packedDir{Path: migrate.Tilde(cfg.Root, m.Home), Files: homeSt.Files, Bytes: homeSt.Bytes},
		Extras:        []packedDir{}, ExtrasMissing: tildeAll(missing, m.Home), Needed: []neededPath{},
		StayBehind: migrate.StayBehind(), Total: homeSt, Busy: s.migrateBusy(), Exporting: m.current(),
	}
	for _, dir := range extras {
		st, err := migrate.Measure(dir, nil)
		if err != nil {
			return nil, err
		}
		plan.Extras = append(plan.Extras, packedDir{Path: migrate.Tilde(dir, m.Home), Files: st.Files, Bytes: st.Bytes})
		plan.Total.Files += st.Files
		plan.Total.Bytes += st.Bytes
	}
	for _, p := range migrate.NotPacked(migrate.NeededPaths(cfg.Root, m.Home), extras) {
		_, statErr := os.Stat(p)
		plan.Needed = append(plan.Needed, neededPath{Path: migrate.Tilde(p, m.Home), Exists: statErr == nil})
	}
	return plan, nil
}

func (s *Server) migrateBusy() migrateBusy {
	b := migrateBusy{Pending: len(s.Engine.Pending())}
	for _, r := range s.Engine.Running() {
		if r.Job != "" {
			b.Jobs++
		} else {
			b.Rounds++
		}
	}
	return b
}

// migrateExport starts `vigil export` detached. It finishes the work in progress, stops this daemon for
// good and packs everything, so it needs an explicit confirm.
func (s *Server) migrateExport(_ http.ResponseWriter, r *http.Request) (any, error) {
	m, err := s.migrator()
	if err != nil {
		return nil, err
	}
	var b struct {
		Confirm bool `json:"confirm"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if !b.Confirm {
		return nil, badRequest("搬家会停掉这台电脑上的 Vigil，要带 {\"confirm\": true} 确认")
	}
	cfg, err := m.Load()
	if err != nil {
		return nil, fmt.Errorf("读配置：%w", err)
	}
	started, err := m.begin(cfg.Root, s.Now())
	if err != nil {
		return nil, err
	}
	s.logf("migrate: export started by the console, packing to %s (log %s)", started.Path, started.Log)
	return started, nil
}

// begin starts the export unless one is still going.
func (m *Migrator) begin(root string, now time.Time) (*exportStarted, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.goingLocked() {
		return nil, &httpError{code: http.StatusConflict, msg: "已经在打包了：" + m.last.Path}
	}
	if m.Start == nil || m.Bin == "" {
		return nil, errors.New("没有配置怎么启动 vigil export")
	}
	path := archivePath(m.Home, now)
	logPath := path + exportLogSuffix
	env := append(os.Environ(), "VIGIL_HOME="+root)
	done, err := m.Start(m.Bin, []string{"export", "-o", path}, env, logPath)
	if err != nil {
		return nil, fmt.Errorf("启动 vigil export：%w", err)
	}
	m.running = done
	m.last = &exportStarted{Path: path, Log: logPath, ImportCommand: importCommand(path, m.Home), StartedAt: now}
	cp := *m.last
	return &cp, nil
}

// current returns the export still going, if any.
func (m *Migrator) current() *exportStarted {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.goingLocked() {
		return nil
	}
	cp := *m.last
	return &cp
}

func (m *Migrator) goingLocked() bool {
	if m.running == nil {
		return false
	}
	select {
	case <-m.running:
		return false
	default:
		return true
	}
}

func archivePath(home string, now time.Time) string {
	return filepath.Join(home, "vigil-export-"+now.Format("20060102-1504")+".tar.gz")
}

// importCommand is what to run in the Vigil repo on the new machine once the pack is copied to its home.
func importCommand(archive, home string) string {
	return "bin/vigil import ~/" + filepath.Base(archive)
}

func tildeAll(paths []string, home string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, migrate.Tilde(p, home))
	}
	return out
}

// StartDetached runs bin in its own session with its output in logPath, so it survives this daemon
// being stopped (which `vigil export` does). done closes when it exits.
func StartDetached(bin string, args, env []string, logPath string) (<-chan struct{}, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("建日志 %s：%w", logPath, err)
	}
	defer f.Close()
	cmd := osexec.Command(bin, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, f, f
	cmd.Dir = filepath.Dir(logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done, nil
}
