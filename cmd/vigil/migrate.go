package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/migrate"
)

// Moving to another computer: vigil export on the old one (finishes work in progress, stops the service
// and marks it moved, so two machines never answer the same messages), vigil import on the new one
// (unpacks, rewrites paths, checks every tool and login, and starts only once all pass).

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	out := fs.String("o", filepath.Join(home, "vigil-export-"+time.Now().Format("20060102-1504")+".tar.gz"), "包的路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	label = cfg.LaunchdLabel
	extras, missing, err := migrate.ExpandExtras(cfg.MigrateExtra, home)
	if err != nil {
		return err
	}
	for _, p := range missing {
		fmt.Printf("提示：migrate_extra 里的 %s 不存在，跳过\n", p)
	}
	wasRunning := loaded()
	if wasRunning {
		fmt.Println("先让进行中的轮次和后台子任务跑完，再停服务打包……")
		drainBeforeRestart()
		if out, err := launchctl("bootout", domain()+"/"+label); err != nil {
			return fmt.Errorf("停止服务失败：%v：%s", err, strings.TrimSpace(string(out)))
		}
	}
	host, _ := os.Hostname()
	m := migrate.Manifest{ExportedAt: time.Now(), Host: host, VigilVersion: version, Home: home, Root: cfg.Root}
	st, err := writeArchive(*out, m, extras)
	if err != nil {
		if wasRunning {
			if pp, perr := plistPath(); perr == nil {
				_ = restartService(pp)
			}
		}
		return fmt.Errorf("%w（服务已恢复运行）", err)
	}
	if err := migrate.MarkMovedOut(cfg.Root, *out, time.Now()); err != nil {
		return err
	}
	if pp, err := plistPath(); err == nil {
		_ = os.Remove(pp)
	}
	printExportSummary(*out, st, cfg, extras, home)
	return nil
}

func writeArchive(path string, m migrate.Manifest, extras []string) (migrate.Stats, error) {
	tmp := path + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return migrate.Stats{}, err
	}
	st, err := migrate.Export(f, m, extras)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return st, err
	}
	return st, os.Rename(tmp, path)
}

func printExportSummary(path string, st migrate.Stats, cfg config.Config, extras []string, home string) {
	fmt.Printf("\n已打包：%s（%d 个文件，%.1f MB）\n", path, st.Files, float64(st.Bytes)/(1<<20))
	if len(extras) > 0 {
		fmt.Println("连同任务在 Vigil 目录外的状态：" + strings.Join(tilde(extras, home), "、"))
	}
	fmt.Println("这台电脑上的 Vigil 已停止，不会再自动启动。")
	fmt.Println("没打包：控制台 token（新电脑会重新生成）、各平台登录、代码仓库、命令行工具。")
	if need := migrate.NotPacked(migrate.NeededPaths(cfg.Root, home), extras); len(need) > 0 {
		fmt.Println("\n任务还用到这些路径，新电脑上要有（仓库要 clone，工具要装）：")
		for _, p := range tilde(need, home) {
			fmt.Println("  " + p)
		}
	}
	fmt.Println("\n新电脑上：clone Vigil 仓库，make build，然后 bin/vigil import <包>")
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	force := fs.Bool("force", false, "这台电脑已有 Vigil 数据时，先挪到一边再导入")
	noInstall := fs.Bool("no-install", false, "只解包和体检，不安装启动")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法：vigil import <vigil export 打的包> [--force] [--no-install]")
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if *force {
		if err := moveAside(root); err != nil {
			return err
		}
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	m, err := migrate.Import(f, root, home)
	if errors.Is(err, migrate.ErrExists) {
		return fmt.Errorf("%w。这台电脑已经有 Vigil 数据，确认不要了就加 --force（会先挪到一边，不删除）", err)
	}
	if err != nil {
		return err
	}
	fmt.Printf("已从 %s（%s 导出）恢复到 %s\n", m.Host, m.ExportedAt.Local().Format("2006-01-02 15:04"), root)
	if m.Home != home {
		fmt.Printf("路径已从 %s 改成 %s\n", m.Home, home)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg = config.RepinTools(cfg)
	if err := config.Save(cfg); err != nil {
		return err
	}
	if bad := doctor(cfg, home); bad > 0 {
		fmt.Printf("\n还有 %d 项没就绪，常驻值守先不启动。按上面的提示补齐后跑 vigil doctor，全部通过再 make install。\n", bad)
		return nil
	}
	if *noInstall {
		fmt.Println("\n全部就绪。跑 make install 启动。")
		return nil
	}
	return installAfterImport(cfg)
}

// moveAside renames an existing Vigil home out of the way, keeping it in case it was wanted after all.
func moveAside(root string) error {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	aside := root + ".before-import-" + time.Now().Format("20060102-150405")
	if err := os.Rename(root, aside); err != nil {
		return fmt.Errorf("挪开已有数据：%w", err)
	}
	fmt.Println("已有的 Vigil 数据挪到了 " + aside)
	return nil
}

// installAfterImport builds, signs and starts Vigil from its source repo (make install), falling back to
// installing this binary when the repo is not where the config says.
func installAfterImport(cfg config.Config) error {
	if cfg.SourceDir != "" {
		if _, err := os.Stat(filepath.Join(cfg.SourceDir, "Makefile")); err == nil {
			fmt.Println("\n全部就绪，在 " + cfg.SourceDir + " 跑 make install……")
			cmd := osexec.Command("make", "install")
			cmd.Dir, cmd.Stdout, cmd.Stderr = cfg.SourceDir, os.Stdout, os.Stderr
			return cmd.Run()
		}
	}
	fmt.Println("\n全部就绪，安装启动……")
	return cmdInstall(nil)
}

func cmdDoctor(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if bad := doctor(cfg, home); bad > 0 {
		return fmt.Errorf("%d 项没就绪", bad)
	}
	fmt.Println("\n全部就绪。")
	return nil
}

// doctor checks what Vigil needs on this machine and prints one line per item; it returns how many failed.
func doctor(cfg config.Config, home string) int {
	fmt.Println("\n体检：")
	findings := migrate.Doctor(context.Background(), cfg, home)
	for _, f := range findings {
		if f.OK {
			fmt.Println("  ✓ " + f.Name)
			continue
		}
		fmt.Println("  ✗ " + f.Name)
		if hint := strings.TrimSpace(strings.Join(nonEmpty(f.Detail, f.Hint), "\n")); hint != "" {
			fmt.Println("      " + strings.ReplaceAll(hint, "\n", "\n      "))
		}
	}
	return migrate.Failed(findings)
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func tilde(paths []string, home string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = migrate.Tilde(p, home)
	}
	return out
}
