// Package migrate packs a Vigil home into one archive and unpacks it on another machine, so moving to a
// new computer is "copy one file, then log in again". Logins and the API token never travel: they are
// credentials, some tied to the device, and a new token is made on first start.
package migrate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	formatVersion = 1
	manifestName  = "manifest.json"
	rootPrefix    = "vigil/"
	homePrefix    = "home/"
	// MovedOutFile marks a Vigil home that was exported: this machine must not start it again, or both
	// machines would answer the same Slack messages.
	MovedOutFile = "MOVED_OUT.json"
	// maxRewrite bounds the files searched for the old home path; larger ones are logs, not settings.
	maxRewrite = 16 << 20
	sniffBytes = 8 << 10
)

// ErrExists means the target already holds a Vigil home; importing over it would mix two machines' state.
var ErrExists = errors.New("target already has a Vigil home")

// stayBehind are files in the Vigil home that never travel: the console token (a new one is made), the
// daemon's lock and logs, and the moved-out marker itself.
var stayBehind = map[string]bool{"token": true, "daemon.lock": true, "daemon.out": true, MovedOutFile: true}

func staysBehind(rel string) bool {
	return stayBehind[rel] || strings.HasPrefix(rel, "vigil.log")
}

// Manifest describes an archive.
type Manifest struct {
	Version      int       `json:"version"`
	ExportedAt   time.Time `json:"exported_at"`
	Host         string    `json:"host,omitempty"`
	VigilVersion string    `json:"vigil_version,omitempty"`
	Home         string    `json:"home"`
	Root         string    `json:"root"`
	// Extras are the state directories outside the Vigil home that tasks keep (a cursor, say), relative
	// to Home so they land under the new machine's home.
	Extras []string `json:"extras,omitempty"`
}

// Stats counts what an export packed.
type Stats struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Export writes m.Root and the extra directories (absolute, each inside m.Home) as a gzip'd tar to w.
func Export(w io.Writer, m Manifest, extras []string) (Stats, error) {
	m.Version = formatVersion
	m.Extras = nil
	for _, dir := range extras {
		rel, err := filepath.Rel(m.Home, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return Stats{}, fmt.Errorf("%s 不在 %s 下，换了电脑没法放回同一个位置", dir, m.Home)
		}
		m.Extras = append(m.Extras, filepath.ToSlash(rel))
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	head, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Stats{}, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(head)), ModTime: m.ExportedAt, Typeflag: tar.TypeReg}); err != nil {
		return Stats{}, err
	}
	if _, err := tw.Write(head); err != nil {
		return Stats{}, err
	}
	var st Stats
	if err := addTree(tw, m.Root, rootPrefix, staysBehind, &st); err != nil {
		return st, err
	}
	for i, dir := range extras {
		if err := addTree(tw, dir, homePrefix+m.Extras[i]+"/", nil, &st); err != nil {
			return st, err
		}
	}
	if err := tw.Close(); err != nil {
		return st, err
	}
	return st, gz.Close()
}

// addTree adds the regular files and directories under dir; symlinks, sockets and the like are skipped.
func addTree(tw *tar.Writer, dir, prefix string, skip func(rel string) bool, st *Stats) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("读 %s：%w", path, err)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: prefix + rel + "/", Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Typeflag: tar.TypeDir})
		case info.Mode().IsRegular():
			return addFile(tw, path, prefix+rel, info, st)
		default:
			return nil
		}
	})
}

func addFile(tw *tar.Writer, path, name string, info fs.FileInfo, st *Stats) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("打包 %s：%w", path, err)
	}
	st.Files++
	st.Bytes += n
	return nil
}

// Import unpacks an archive: the Vigil home into root, the extras under home. When the archive came from
// a machine with another home directory, that path is rewritten in the unpacked text files.
func Import(r io.Reader, root, home string) (Manifest, error) {
	if _, err := os.Stat(filepath.Join(root, "config.yaml")); err == nil {
		return Manifest{}, fmt.Errorf("%w：%s", ErrExists, root)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, fmt.Errorf("不是 vigil export 打的包：%w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var m Manifest
	seen := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("读包：%w", err)
		}
		if h.Name == manifestName {
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return m, fmt.Errorf("读包说明：%w", err)
			}
			if m.Version != formatVersion {
				return m, fmt.Errorf("包格式版本 %d，这个 vigil 只认 %d，先把两边升到同一版本", m.Version, formatVersion)
			}
			seen = true
			continue
		}
		if !seen {
			return m, errors.New("包里缺少说明文件，不是 vigil export 打的包")
		}
		dest, err := target(h.Name, root, home)
		if err != nil {
			return m, err
		}
		if err := unpack(tr, h, dest); err != nil {
			return m, err
		}
	}
	if !seen {
		return m, errors.New("空包")
	}
	if m.Home != "" && m.Home != home {
		dirs := []string{root}
		for _, e := range m.Extras {
			dirs = append(dirs, filepath.Join(home, filepath.FromSlash(e)))
		}
		if err := RewriteHome(dirs, m.Home, home); err != nil {
			return m, err
		}
	}
	return m, nil
}

// target maps an archive entry to its place on this machine, refusing anything that would land outside.
func target(name, root, home string) (string, error) {
	var base, rel string
	switch {
	case strings.HasPrefix(name, rootPrefix):
		base, rel = root, strings.TrimPrefix(name, rootPrefix)
	case strings.HasPrefix(name, homePrefix):
		base, rel = home, strings.TrimPrefix(name, homePrefix)
	default:
		return "", fmt.Errorf("包里有不认识的条目：%s", name)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("包里的条目跑出了目标目录，拒绝解开：%s", name)
	}
	return filepath.Join(base, clean), nil
}

func unpack(tr *tar.Reader, h *tar.Header, dest string) error {
	mode := fs.FileMode(h.Mode).Perm()
	switch h.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(dest, mode|0o700)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return fmt.Errorf("解开 %s：%w", dest, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Chtimes(dest, h.ModTime, h.ModTime)
	default:
		return fmt.Errorf("包里有不支持的条目类型：%s", h.Name)
	}
}

// RewriteHome replaces oldHome with newHome in the text files under dirs (task docs, config, scripts,
// state). Binary files and anything over maxRewrite are left alone.
func RewriteHome(dirs []string, oldHome, newHome string) error {
	from, to := []byte(oldHome), []byte(newHome)
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() > maxRewrite {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.IndexByte(data[:min(len(data), sniffBytes)], 0) >= 0 || !bytes.Contains(data, from) {
				return nil
			}
			if err := os.WriteFile(path, bytes.ReplaceAll(data, from, to), info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(path, info.ModTime(), info.ModTime())
		})
		if err != nil {
			return fmt.Errorf("改写 %s 里的路径：%w", dir, err)
		}
	}
	return nil
}

// pathRef finds home paths written as ~/…; NeededPaths also looks for the absolute home.
var pathRef = regexp.MustCompile(`~(/[A-Za-z0-9._\-/]+)`)

// NeededPaths lists what the tasks expect on this machine outside the Vigil home: every home path that
// config.yaml, the scripts and the task docs mention, cut to its first two levels (~/.watcher/state,
// ~/LB/repo), sorted.
func NeededPaths(root, home string) []string {
	files := []string{filepath.Join(root, "config.yaml")}
	for _, pat := range []string{"tasks/*/brief.md", "tasks/*/runbook.md", "tasks/*/task.yaml", "scripts/*"} {
		m, _ := filepath.Glob(filepath.Join(root, pat))
		files = append(files, m...)
	}
	abs := regexp.MustCompile(regexp.QuoteMeta(home) + `(/[A-Za-z0-9._\-/]+)`)
	seen := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, re := range []*regexp.Regexp{pathRef, abs} {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				if p := topTwo(home, m[1]); p != "" && !within(p, root) {
					seen[p] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func topTwo(home, rel string) string {
	parts := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' })
	for len(parts) > 0 && strings.Trim(parts[len(parts)-1], ".") == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return ""
	}
	parts = parts[:min(len(parts), 2)]
	parts[len(parts)-1] = strings.TrimRight(parts[len(parts)-1], ".")
	return filepath.Join(append([]string{home}, parts...)...)
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
