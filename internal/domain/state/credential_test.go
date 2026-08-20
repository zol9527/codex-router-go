package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 环境文件兜底红线：GUI App 拉起的服务没有登录 shell 的环境，
// {VAR} 引用必须能从 config.toml [env].file 指向的 dotenv 文件解析；
// 进程环境仍然优先；两条路都没有才算未配置。
func TestReadConfigCredentialEnvFileFallback(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(envFile, []byte("export GLM_TEST_KEY=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Join([]string{
		"[env]",
		"file = \"" + envFile + "\"",
		"",
		"[zai-coding]",
		"api_key = \"{GLM_TEST_KEY}\"",
		"",
		"[opencode-go]",
		"api_key = \"{TOTALLY_MISSING_KEY}\"",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	v, ok := st.ReadConfigCredential("zai-coding")
	if !ok || v != "from-file" {
		t.Fatalf("zai-coding = (%q,%v), want (from-file,true)", v, ok)
	}
	if _, ok := st.ReadConfigCredential("opencode-go"); ok {
		t.Fatal("missing key in both env and file must be unconfigured")
	}
	if p, n := DanglingEnvRef(); p != "opencode-go" || n != "TOTALLY_MISSING_KEY" {
		t.Fatalf("dangling ref = (%q,%q)", p, n)
	}

	// 进程环境优先于环境文件（开发覆盖语义不变）。
	t.Setenv("GLM_TEST_KEY", "from-env")
	if v, _ := st.ReadConfigCredential("zai-coding"); v != "from-env" {
		t.Fatalf("process env should win, got %q", v)
	}
}

// [env].file 支持 ~ 路径；文件缺失时兜底静默为空，不影响其他表。
func TestReadConfigCredentialEnvFileTildeAndMissing(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := "[env]\nfile = \"~/mr-no-such-secrets.env\"\n\n[zai-coding]\napi_key = \"literal-key\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	v, ok := st.ReadConfigCredential("zai-coding")
	if !ok || v != "literal-key" {
		t.Fatalf("literal = (%q,%v); missing env file must not break literals", v, ok)
	}
}
