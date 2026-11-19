package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"time"

	"vigil/internal/config"
	"vigil/internal/migrate"
)

// label is the launchd service name, from config.yaml (launchd_label); set before any launchctl call.
var label = "dev.vigil.daemon"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func renderPlist(bin, root, path, home string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + label + `</string>
  <key>ProgramArguments</key>
  <array><string>` + xmlEscape(bin) + `</string><string>daemon</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ProcessType</key><string>Interactive</string>
  <key>WorkingDirectory</key><string>` + xmlEscape(home) + `</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>` + xmlEscape(path) + `</string>
    <key>HOME</key><string>` + xmlEscape(home) + `</string>
  </dict>
  <key>StandardOutPath</key><string>` + xmlEscape(filepath.Join(root, "daemon.out")) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(root, "daemon.out")) + `</string>
</dict>
</plist>
`
}

func launchctl(args ...string) ([]byte, error) {
	return osexec.Command("launchctl", args...).CombinedOutput()
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func cmdInstall(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	label = cfg.LaunchdLabel
	if err := migrate.RefuseMovedOut(cfg.Root); err != nil {
		return err
	}
	if err := requireHerdr(cfg.HerdrBin); err != nil {
		return err
	}
	// Pin absolute tool paths now: launchd starts with a minimal PATH.
	if err := config.Save(cfg); err != nil {
		return err
	}
	pp, err := plistPath()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(pp, []byte(renderPlist(selfPath(), cfg.Root, os.Getenv("PATH"), home)), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if loaded() && !hasFlag(args, "--force") {
		drainBeforeRestart()
	}
	if err := restartService(pp); err != nil {
		return err
	}
	fmt.Printf("Vigil 已安装并启动（%s）\n控制台：%s\n日志：%s\n", pp, cfg.URL(), filepath.Join(cfg.Root, "vigil.log"))
	if cfg.Slack.Enabled() && !hasFlag(args, "--no-selftest") {
		selfTestAfterInstall()
	}
	return nil
}

const (
	bootoutWait     = 15 * time.Second
	bootstrapTries  = 5
	bootstrapPause  = time.Second
	servicePollStep = 200 * time.Millisecond
)

func loaded() bool {
	_, err := launchctl("print", domain()+"/"+label)
	return err == nil
}

// restartService unloads the old job, waits until launchd has really let go of it (bootout is
// asynchronous), then loads the new plist, retrying the transient "Input/output error".
func restartService(plist string) error {
	if loaded() {
		_, _ = launchctl("bootout", domain()+"/"+label)
		deadline := time.Now().Add(bootoutWait)
		for loaded() && time.Now().Before(deadline) {
			time.Sleep(servicePollStep)
		}
	}
	var last error
	for i := 0; i < bootstrapTries; i++ {
		out, err := launchctl("bootstrap", domain(), plist)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("launchctl bootstrap: %w: %s", err, bytes.TrimSpace(out))
		time.Sleep(bootstrapPause)
	}
	return fmt.Errorf("%w（服务现在可能是停着的，手动执行：launchctl bootstrap %s %s）", last, domain(), plist)
}

func cmdUninstall(_ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	label = cfg.LaunchdLabel
	pp, err := plistPath()
	if err != nil {
		return err
	}
	if out, err := launchctl("bootout", domain()+"/"+label); err != nil {
		fmt.Printf("launchctl bootout: %s\n", bytes.TrimSpace(out))
	}
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Vigil 服务已移除（数据目录保留）")
	return nil
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

const herdrInstall = "curl -fsSL https://herdr.dev/install.sh | sh"

// requireHerdr refuses to install without herdr: new tasks are talked through, and runs taken over, in it.
func requireHerdr(bin string) error {
	if _, err := osexec.LookPath(bin); err != nil {
		return fmt.Errorf("没找到 herdr（%s）。Vigil 新建任务、接管会话都在 herdr 里进行，没有它用不了。先安装：%s，再重新运行 vigil install", bin, herdrInstall)
	}
	return nil
}
