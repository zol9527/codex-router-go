package cred

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

func testProvider() *registry.Provider {
	return &registry.Provider{
		ID: "zai-coding",
		Credential: registry.Credential{
			Environment:      []string{"ZAI_TEST_KEY"},
			File:             "zai-coding-api-key.secret",
			KeychainServices: nil, // 测试不碰 Keychain
		},
	}
}

func openState(t *testing.T) *state.State {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// 顺序：env > config.toml > .secret 文件。env 是开发覆盖，config 是
// 操作者的家，.secret 是历史来源。
func TestResolveOrder(t *testing.T) {
	st := openState(t)
	r := New(st)
	p := testProvider()

	// 什么都没有。
	if v, src := r.Resolve(p); v != "" || src != "" {
		t.Fatalf("empty resolve = %q %q", v, src)
	}

	// 只有 .secret 文件 → file。
	st.WriteCredentialFile(p.Credential.File, "secret-value")
	if v, src := r.Resolve(p); v != "secret-value" || src != "file" {
		t.Fatalf("file source = %q %q", v, src)
	}

	// 写入 config.toml → config 胜过 .secret。
	if err := st.WriteConfigCredential("zai-coding", "config-value"); err != nil {
		t.Fatal(err)
	}
	if v, src := r.Resolve(p); v != "config-value" || src != "config" {
		t.Fatalf("config source = %q %q", v, src)
	}

	// env 一出现就压倒一切。
	t.Setenv("ZAI_TEST_KEY", "env-value")
	if v, src := r.Resolve(p); v != "env-value" || src != "environment" {
		t.Fatalf("env source = %q %q", v, src)
	}
}

// 变体（opencode-go-responses）必须落在家族主项的表上 ——
// 一张凭证，两种协议。
func TestVariantResolvesFamilyTable(t *testing.T) {
	st := openState(t)
	r := New(st)
	if err := st.WriteConfigCredential("opencode-go", "family-key"); err != nil {
		t.Fatal(err)
	}
	variant := &registry.Provider{
		ID:        "opencode-go-responses",
		VariantOf: "opencode-go",
	}
	if v, src := r.Resolve(variant); v != "family-key" || src != "config" {
		t.Fatalf("variant resolve = %q %q", v, src)
	}
}

// 坏的 config.toml 按无凭证处理（fail-closed），绝不带病猜测。
func TestBrokenConfigYieldsNothing(t *testing.T) {
	st := openState(t)
	r := New(st)
	if err := os.WriteFile(st.ConfigPath(), []byte("[zai-coding\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, src := r.Resolve(testProvider()); v != "" || src != "" {
		t.Fatalf("broken config must yield nothing, got %q %q", v, src)
	}
}

// 注释掉的模板键不算配置（模板里的 "# api_key = ..." 不产生凭证）。
func TestCommentedTemplateKeyIsNotConfigured(t *testing.T) {
	st := openState(t)
	r := New(st)
	os.WriteFile(st.ConfigPath(), []byte("[zai-coding]\n# api_key = \"sk-...\"\n"), 0o600)
	if v, src := r.Resolve(testProvider()); v != "" || src != "" {
		t.Fatalf("commented key must not resolve, got %q %q", v, src)
	}
}
