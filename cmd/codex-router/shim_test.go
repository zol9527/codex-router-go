package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 渲染：marker 在、每条失败路径都 exec 真 codex、旁路变量在。
func TestRenderShim(t *testing.T) {
	script := renderShim("/usr/local/bin/codex", "/checkout", 4202)
	if !strings.Contains(script, shimMarker) {
		t.Error("marker missing")
	}
	if !strings.Contains(script, `exec "$CODEX_BIN" "$@"`) {
		t.Error("must exec the real codex")
	}
	if !strings.Contains(script, "MODEL_ROUTER_SHIM:-1") {
		t.Error("escape hatch missing")
	}
	if !strings.Contains(script, "starting Codex anyway") {
		t.Error("router-down path must still start codex")
	}
	if !strings.Contains(script, `ROUTER_WAIT=${MODEL_ROUTER_SHIM_WAIT:-45}`) {
		t.Error("bounded wait missing")
	}
}

// 红线：绝不覆盖不带 marker 的 codex。
func TestShimRefusesForeignWrapper(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, shimCommand)
	os.WriteFile(wrapper, []byte("#!/bin/sh\nexec /opt/codex \"$@\"\n"), 0o755)
	if isShimFile(wrapper) {
		t.Fatal("foreign wrapper must not be recognized as ours")
	}
	// install 逻辑在 shimTargetDir + 显式检查；这里验证判定原语。
	// （完整 install 测试需要 PATH 操纵，由冒烟覆盖。）
}

// isShimFile：marker 识别。
func TestIsShimFile(t *testing.T) {
	dir := t.TempDir()
	ours := filepath.Join(dir, "ours")
	os.WriteFile(ours, []byte("#!/bin/bash\n# "+shimMarker+"\n"), 0o755)
	if !isShimFile(ours) {
		t.Error("marker file must be recognized")
	}
	foreign := filepath.Join(dir, "foreign")
	os.WriteFile(foreign, []byte("#!/bin/bash\n"), 0o755)
	if isShimFile(foreign) {
		t.Error("foreign file must not be recognized")
	}
}

// shimTargetDir：home 内目录优先，且不越出 home。
func TestShimTargetDirStaysInHome(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	t.Setenv("PATH", "/usr/local/bin:"+bin)
	// 真 codex 在 /usr/local/bin；home 内目录在其后仍可用。
	dir, _ := shimTargetDir(home, "/usr/local/bin/codex")
	if !strings.HasPrefix(dir, home) {
		t.Errorf("target dir %s escapes home %s", dir, home)
	}
}
