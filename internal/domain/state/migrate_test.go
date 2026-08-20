package state

import (
	"os"
	"path/filepath"
	"testing"
)

// 状态目录从 ~/.codex/codex-router（原 Node 栈，嵌在 Codex 家目录里）
// 迁到独立的 ~/.codex-router。迁移的每条边界都有失败场景在背后：
// 覆盖操作者已有状态、误删旧目录、重发布 config.toml。

func writeLegacyState(t *testing.T, home string) string {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_ROUTER_STATE_DIR", "")
	legacy := filepath.Join(home, ".codex", "codex-router")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"caller-secret":          "caller-key-0123456789abcdefghijklmnop",
		"internal-secret":        "internal-key-0123456789abcdefghijklm",
		"enabled-providers.json": `["zai-coding","opencode-go"]`,
		"presence.json":          `{"mode":"always"}`,
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return legacy
}

func TestOpenMigratesLegacyStateDir(t *testing.T) {
	home := t.TempDir()
	legacy := writeLegacyState(t, home)

	st, err := Open(filepath.Join(home, ".codex-router"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Dir != filepath.Join(home, ".codex-router") {
		t.Fatalf("state dir = %q", st.Dir)
	}
	// caller-secret 随迁：config.toml 已发布的 base URL 保持有效。
	key, err := st.CallerKey()
	if err != nil || key != "caller-key-0123456789abcdefghijklmnop" {
		t.Fatalf("caller key after migration = %q err=%v", key, err)
	}
	raw, err := os.ReadFile(filepath.Join(st.Dir, "enabled-providers.json"))
	if err != nil || string(raw) == "" {
		t.Fatalf("enabled-providers.json not migrated: %v", err)
	}
	// 旧目录原样保留。
	if _, err := os.Stat(filepath.Join(legacy, "caller-secret")); err != nil {
		t.Fatalf("legacy dir must stay untouched: %v", err)
	}
	// 临时文件不残留。
	entries, _ := os.ReadDir(st.Dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".migrating" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// 新目录已存在 = 不是首次：操作者自己的状态优先，绝不自动写入。
func TestOpenSkipsMigrationWhenTargetExists(t *testing.T) {
	home := t.TempDir()
	writeLegacyState(t, home)
	target := filepath.Join(home, ".codex-router")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "caller-secret"),
		[]byte("operator-own-key-0123456789abcdefghij"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := st.CallerKey()
	if key != "operator-own-key-0123456789abcdefghij" {
		t.Fatalf("operator state overwritten: %q", key)
	}
	if _, err := os.Stat(filepath.Join(target, "enabled-providers.json")); !os.IsNotExist(err) {
		t.Error("migration must not run when target exists")
	}
}

// 无旧目录 = 全新安装：不产生迁移副作用。
func TestOpenWithoutLegacyIsCleanInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_ROUTER_STATE_DIR", "")

	st, err := Open(filepath.Join(home, ".codex-router"))
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(st.Dir)
	if len(entries) != 0 {
		t.Errorf("fresh install must start empty, got %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Error("no legacy dir must be created")
	}
}

// DefaultDir 的环境覆盖仍然最高优先。
func TestDefaultDirEnvOverrideWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_ROUTER_STATE_DIR", filepath.Join(home, "custom-state"))
	if got := DefaultDir(); got != filepath.Join(home, "custom-state") {
		t.Fatalf("DefaultDir = %q", got)
	}
}
