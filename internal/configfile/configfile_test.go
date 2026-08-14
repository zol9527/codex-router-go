package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const userToml = `# user's own config
model = "gpt-5.5"
model_provider = "openai"

[projects."~/work"]
trust_level = "trusted"

[mcp_servers.github]
command = "npx"
`

// 集成：两个标记块写入，用户内容原样保留，幂等。
func TestInstallIdempotent(t *testing.T) {
	path := writeTemp(t, userToml)
	cfg := RouterConfig{
		BaseURL:     "http://127.0.0.1:4202/_codex-router/secretsecretsecretsecret/v1",
		CatalogPath: "/state/merged-models.json",
	}
	if err := Install(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	afterFirst := string(raw)
	for _, want := range []string{
		startMarker, endMarker, providerStart, providerEnd,
		`[model_providers.codex-router]`, `wire_api = "responses"`,
		`model = "gpt-5.5"`, // 用户内容保留
		`trust_level = "trusted"`,
		`[mcp_servers.github]`,
	} {
		if !strings.Contains(afterFirst, want) {
			t.Errorf("missing %q in:\n%s", want, afterFirst)
		}
	}
	// 根级块必须在第一个表头之前。
	rootIdx := strings.Index(afterFirst, startMarker)
	tableIdx := strings.Index(afterFirst, `[projects."~/work"]`)
	if rootIdx == -1 || tableIdx == -1 || rootIdx > tableIdx {
		t.Errorf("root block must precede first table header")
	}

	// 再装一次：不重复。
	if err := Install(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if got := strings.Count(string(raw), startMarker); got != 1 {
		t.Errorf("root block duplicated: %d", got)
	}
	if got := strings.Count(string(raw), providerStart); got != 1 {
		t.Errorf("provider block duplicated: %d", got)
	}
}

// 拒绝覆盖用户自有的 openai_base_url。
func TestRefusesUserOwnedFields(t *testing.T) {
	path := writeTemp(t, "model = \"gpt-5.5\"\nopenai_base_url = \"https://user.example/v1\"\n")
	err := Install(path, RouterConfig{BaseURL: "http://x", CatalogPath: "/c"})
	if err == nil {
		t.Fatal("must refuse user-owned openai_base_url")
	}
	// 文件未被动过。
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "https://user.example/v1") {
		t.Error("file must be untouched on refusal")
	}
}

// 卸载还原到与安装前等价（用户内容原样，管理块消失）。
func TestUninstallRestores(t *testing.T) {
	path := writeTemp(t, userToml)
	if err := Install(path, RouterConfig{BaseURL: "http://x", CatalogPath: "/c"}); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	after := string(raw)
	for _, banned := range []string{startMarker, endMarker, providerStart, providerEnd, "codex-router"} {
		if strings.Contains(after, banned) {
			t.Errorf("residue %q after uninstall:\n%s", banned, after)
		}
	}
	for _, want := range []string{`model = "gpt-5.5"`, `trust_level = "trusted"`, `[mcp_servers.github]`} {
		if !strings.Contains(after, want) {
			t.Errorf("user content lost: %q", want)
		}
	}
}

// Status 读回集成值。
func TestStatus(t *testing.T) {
	path := writeTemp(t, userToml)
	cfg := RouterConfig{
		BaseURL:     "http://127.0.0.1:4202/_codex-router/secretsecretsecretsecret/v1",
		CatalogPath: "/state/merged-models.json",
	}
	if err := Install(path, cfg); err != nil {
		t.Fatal(err)
	}
	installed, baseURL, catalogPath := Status(path)
	if !installed || baseURL != cfg.BaseURL || catalogPath != cfg.CatalogPath {
		t.Errorf("status = %v %q %q", installed, baseURL, catalogPath)
	}
}
