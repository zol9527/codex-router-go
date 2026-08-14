package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// 加载仓库真实的 config/ 树：证明 Go 加载器与 Node 注册表格式完全兼容。
func TestLoadRealConfigTree(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "..", "..", "config")
	if _, err := os.Stat(configDir); err != nil {
		t.Skip("config/ not found (running outside repo)")
	}
	reg, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	// 本 fork 的三个 provider 必须完整出现。
	for _, id := range []string{"zai-coding", "opencode-go", "opencode-go-responses"} {
		if reg.Providers[id] == nil {
			t.Errorf("provider %s missing", id)
		}
	}
	// 关键模型。
	if m := reg.ForSlug("zai-coding/glm-5.3"); m == nil {
		t.Error("zai-coding/glm-5.3 missing")
	} else {
		if m.UpstreamModel != "glm-5.3" {
			t.Errorf("upstream model = %q", m.UpstreamModel)
		}
		if m.RequestProfile != "glm-thinking" {
			t.Errorf("request profile = %q", m.RequestProfile)
		}
		if m.ContextWindow != 200000 {
			t.Errorf("context window = %d", m.ContextWindow)
		}
		if len(m.ReasoningLevels) != 3 {
			t.Errorf("reasoning levels = %d, want 3", len(m.ReasoningLevels))
		}
	}
	if reg.ForSlug("opencode-go/glm-5.3") == nil {
		t.Error("opencode-go/glm-5.3 missing")
	}
	if m := reg.ForSlug("opencode-go-responses/gpt-5.6-luna"); m == nil {
		t.Error("opencode-go-responses/gpt-5.6-luna missing")
	} else if reg.ProviderFor(m) == nil || reg.ProviderFor(m).Protocol != "openai-responses" {
		t.Error("gpt-5.6-luna provider must be the responses protocol variant")
	}
	// gateway id 索引。
	if reg.ForGatewayModel("zai-coding-glm-5-3") == nil {
		t.Error("gateway id index broken")
	}
	// 变体归并。
	if reg.CanonicalProviderID("opencode-go-responses") != "opencode-go" {
		t.Error("variant should canonicalize to its family")
	}
	if reg.CanonicalProviderID("zai-coding") != "zai-coding" {
		t.Error("primary provider canonicalizes to itself")
	}
}
