package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vigil/internal/config"
	"vigil/internal/health"
)

// Finding kinds, so a console can word each one in its own language.
const (
	KindMovedOut = "moved_out"
	KindCheck    = "check"
	KindSource   = "source"
	KindPath     = "path"
)

// failLines is how much of a failing check's output a finding keeps.
const failLines = 3

// Finding is one item of a health check: something Vigil needs on this machine.
type Finding struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"` // the check's name or the path (shortened to ~/…)
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"` // why it failed (the last lines a check printed, say)
	Hint    string `json:"hint,omitempty"`   // how to fix it
}

// MovedOut is the marker an export leaves behind.
type MovedOut struct {
	At      time.Time `json:"at"`
	Archive string    `json:"archive"`
}

// ReadMovedOut returns the marker of a Vigil home that was exported, or nil when it was not.
func ReadMovedOut(root string) (*MovedOut, error) {
	data, err := os.ReadFile(filepath.Join(root, MovedOutFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读搬家标记：%w", err)
	}
	var m MovedOut
	_ = json.Unmarshal(data, &m) // a damaged marker still means "moved"
	return &m, nil
}

// MarkMovedOut records that this Vigil home went into archive.
func MarkMovedOut(root, archive string, at time.Time) error {
	data, err := json.Marshal(MovedOut{At: at, Archive: archive})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, MovedOutFile), data, 0o600)
}

// RefuseMovedOut stops a Vigil home that was exported from running here again.
func RefuseMovedOut(root string) error {
	m, err := ReadMovedOut(root)
	if err != nil || m == nil {
		return err
	}
	return fmt.Errorf("这台电脑的 Vigil 已在 %s 导出搬走（%s），再启动会和新电脑重复回复。确实要在这里继续用，删掉 %s 再装",
		m.At.Local().Format("2006-01-02 15:04"), m.Archive, filepath.Join(root, MovedOutFile))
}

// Doctor checks what Vigil needs on this machine: the home was not moved away, every login/tool check
// passes, the source repo and every path the tasks mention exist. The checks run side by side, each
// bounded by health.Shell's timeout.
func Doctor(ctx context.Context, cfg config.Config, home string) []Finding {
	var out []Finding
	if err := RefuseMovedOut(cfg.Root); err != nil {
		out = append(out, Finding{Kind: KindMovedOut, Name: "这份数据没有被标记为已搬走", Detail: err.Error()})
	}
	out = append(out, runChecks(ctx, cfg.Checks)...)
	if cfg.SourceDir != "" {
		p := Tilde(cfg.SourceDir, home)
		out = append(out, Finding{Kind: KindSource, Subject: p, Name: "Vigil 源码仓库 " + p, OK: exists(cfg.SourceDir),
			Hint: "clone 到这个位置（自进化提的代码改动要在这里合并安装）"})
	}
	for _, path := range NeededPaths(cfg.Root, home) {
		p := Tilde(path, home)
		out = append(out, Finding{Kind: KindPath, Subject: p, Name: "任务用到的路径 " + p, OK: exists(path),
			Hint: "任务的 brief/runbook 里引用了它：仓库要 clone、工具要装、状态目录要随包带过来（config.yaml 的 migrate_extra）"})
	}
	for i := range out {
		if out[i].OK {
			out[i].Hint = ""
		}
	}
	return out
}

func runChecks(ctx context.Context, checks []health.Check) []Finding {
	out := make([]Finding, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := health.Shell(ctx, c.Command)
			f := Finding{Kind: KindCheck, Subject: c.Name, Name: "登录/工具：" + c.Name, OK: err == nil, Hint: strings.TrimSpace(c.Hint)}
			if err != nil {
				f.Detail = lastLines(res, failLines)
				if f.Detail == "" {
					f.Detail = err.Error()
				}
			}
			out[i] = f
		}()
	}
	wg.Wait()
	return out
}

// Failed counts the findings that did not pass.
func Failed(fs []Finding) int {
	n := 0
	for _, f := range fs {
		if !f.OK {
			n++
		}
	}
	return n
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(strings.Join(lines[max(0, len(lines)-n):], "\n"))
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Tilde shortens a path under home to ~/….
func Tilde(p, home string) string {
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// ExpandExtras resolves config.yaml's migrate_extra (~/… or absolute) into the directories that exist
// and the ones that do not (skipped by an export).
func ExpandExtras(paths []string, home string) (present, missing []string, err error) {
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(p) {
			return nil, nil, fmt.Errorf("migrate_extra 里的 %s 要写成 ~/… 或绝对路径", p)
		}
		if !exists(p) {
			missing = append(missing, p)
			continue
		}
		present = append(present, filepath.Clean(p))
	}
	return present, missing, nil
}

// NotPacked lists the paths that no extra directory covers: the new machine has to provide them.
func NotPacked(paths, extras []string) []string {
	var out []string
	for _, p := range paths {
		covered := false
		for _, e := range extras {
			if within(p, e) || within(e, p) {
				covered = true
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

// Measure counts what an export of root (minus the files that stay behind) and extras would pack.
func Measure(root string, extras []string) (Stats, error) {
	var st Stats
	if err := measureTree(root, staysBehind, &st); err != nil {
		return st, err
	}
	for _, dir := range extras {
		if err := measureTree(dir, nil, &st); err != nil {
			return st, err
		}
	}
	return st, nil
}

func measureTree(dir string, skip func(rel string) bool, st *Stats) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("读 %s：%w", path, err)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		if skip != nil && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st.Files++
		st.Bytes += info.Size()
		return nil
	})
}

// StayBehind lists what never travels in an archive, for a console to show.
func StayBehind() []string {
	return []string{"token", "daemon.lock", "daemon.out", "vigil.log*", MovedOutFile}
}
