package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 自包含安装的两块拷贝：注册表 config/ 与 bin/control 启动器。
// uninstall 删整个安装目录，所以这里只测 install 侧的语义。

func TestCopyConfigTreeReplacesStaleFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// 源：两层目录 + 两个 json。
	os.MkdirAll(filepath.Join(src, "zai", "coding"), 0o755)
	os.WriteFile(filepath.Join(src, "zai", "zai.json"), []byte(`{"v":1}`), 0o644)
	os.WriteFile(filepath.Join(src, "zai", "coding", "glm.json"), []byte(`{"m":1}`), 0o644)

	// 目标已有旧内容：一个会消失的陈旧文件 + 一个会被新内容覆盖的文件。
	os.MkdirAll(filepath.Join(dst, "old-vendor"), 0o755)
	os.WriteFile(filepath.Join(dst, "old-vendor", "removed.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dst, "zai", "zai.json"), []byte(`{"v":0}`), 0o644)

	if err := copyConfigTree(src, dst); err != nil {
		t.Fatal(err)
	}

	if raw, err := os.ReadFile(filepath.Join(dst, "zai", "zai.json")); err != nil || string(raw) != `{"v":1}` {
		t.Errorf("zai.json = %q err=%v (want new content)", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(dst, "zai", "coding", "glm.json")); err != nil || string(raw) != `{"m":1}` {
		t.Errorf("glm.json = %q err=%v", raw, err)
	}
	// 陈旧文件必须随重装消失 —— 注册表会删文件，叠加拷贝会把
	// 已下线的 provider 永远留在安装里。
	if _, err := os.Stat(filepath.Join(dst, "old-vendor")); !os.IsNotExist(err) {
		t.Error("stale entries must be removed on reinstall")
	}
	// 源目录不受影响。
	if _, err := os.Stat(filepath.Join(src, "zai", "zai.json")); err != nil {
		t.Fatalf("source must stay untouched: %v", err)
	}
}

func TestWriteControlLauncherExecutableAndSelfRelative(t *testing.T) {
	dir := t.TempDir()
	if err := writeControlLauncher(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bin", "control")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/control must be executable, mode=%v", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	// 解析同目录二进制（$(dirname $0)/..），不烧死绝对路径 —— 安装目录
	// 整体搬移后启动器仍然工作。
	if !strings.Contains(script, `$(dirname "$0")/../codex-router`) {
		t.Errorf("launcher must resolve the sibling binary, got:\n%s", script)
	}
	// 调试覆盖仍然可用。
	if !strings.Contains(script, "CODEX_ROUTER_GO_BINARY") {
		t.Errorf("launcher must keep the CODEX_ROUTER_GO_BINARY override, got:\n%s", script)
	}
}
