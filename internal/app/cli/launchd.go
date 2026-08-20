package cli

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// App 化后 launchd 不再参与服务生命周期（App 子进程托管服务）。
// 这里只保留对旧安装遗留 plist 的清理，以及共享的小工具。

const launchdLabel = "io.github.codex-router.go"

func launchdPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func selfBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// uninstallLaunchd 清除旧 launchd 布局遗留的 LaunchAgent。
func uninstallLaunchd() error {
	if _, err := os.Stat(launchdPlistPath()); err != nil {
		return nil
	}
	// bootout 是现代形态（按 label 定位、连依赖服务一起终止）；
	// unload 兜底覆盖老版本 launchd。
	exec.Command("/bin/launchctl", "bootout", "gui/"+launchdUID()+"/"+launchdLabel).Run()
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

func launchdUID() string {
	out, err := exec.Command("id", "-u").Output()
	if err != nil {
		return "501"
	}
	return strings.TrimSpace(string(out))
}
