package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"
)

// launchd 集成：单二进制直接作为 LaunchAgent 服务，
// KeepAlive 保活（对应旧栈 start.mjs 整组拉起的职责，这里一个进程全包）。

const launchdLabel = "io.github.codex-router.go"

func homeLibrary() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Library", "LaunchAgents")
}

func launchdPlistPath() string {
	return filepath.Join(homeLibrary(), launchdLabel+".plist")
}

func selfBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.Binary}}</string>
    <string>serve</string>
    <string>--state</string>
    <string>{{.StateDir}}</string>
    <string>--config</string>
    <string>{{.ConfigDir}}</string>
    <string>--port</string>
    <string>{{.Port}}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Adaptive</string>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>{{.LogPath}}</string>
  <key>StandardErrorPath</key>
  <string>{{.LogPath}}</string>
</dict>
</plist>
`

func installLaunchd(port int, stateDir string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd install is macOS-only (run the binary manually on %s)", runtime.GOOS)
	}
	binary, err := selfBinaryPath()
	if err != nil {
		return err
	}
	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	logPath := filepath.Join(absState, "router.log")
	var sb strings.Builder
	tpl := template.Must(template.New("plist").Parse(plistTemplate))
	err = tpl.Execute(&sb, map[string]any{
		"Label":     launchdLabel,
		"Binary":    binary,
		"StateDir":  absState,
		"ConfigDir": defaultConfigDir(),
		"Port":      port,
		"LogPath":   logPath,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(homeLibrary(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(launchdPlistPath(), []byte(sb.String()), 0o644); err != nil {
		return err
	}
	// 先卸旧再装载（幂等刷新）。
	exec.Command("/bin/launchctl", "unload", launchdPlistPath()).Run()
	out, err := exec.Command("/bin/launchctl", "load", launchdPlistPath()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl load: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uninstallLaunchd() error {
	if _, err := os.Stat(launchdPlistPath()); err != nil {
		return nil
	}
	exec.Command("/bin/launchctl", "unload", launchdPlistPath()).Run()
	return os.Remove(launchdPlistPath())
}

// probeHealth 探测本机服务是否回答 /health。
func probeHealth(stateDir string) bool {
	keyRaw, err := os.ReadFile(filepath.Join(stateDir, "caller-secret"))
	if err != nil {
		// /health 无需认证，探到端口即可。
		return probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort()))
	}
	_ = keyRaw
	return probeURL(fmt.Sprintf("http://127.0.0.1:%d/health", defaultPort()))
}

func probeURL(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func codexConfigPath() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ".codex/config.toml"
		}
		home = filepath.Join(userHome, ".codex")
	}
	return filepath.Join(home, "config.toml")
}
