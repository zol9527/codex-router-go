package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

func v2Model(slug string) *registry.Model {
	return &registry.Model{Slug: slug, Provider: "zai-coding", Listed: true, MultiAgentVersion: "v2"}
}

// 本地降权红线：disabled 把 v2 降回 v1，且不改动原注册表对象。
func TestApplySubagentMultiAgent(t *testing.T) {
	proven := v2Model("zai-coding/glm-5.3")
	plain := &registry.Model{Slug: "zai-coding/glm-5.2", Provider: "zai-coding", Listed: true}
	settings := state.SubagentSettings{Version: 2, Mode: state.SubagentModeProven,
		Disabled: []string{"zai-coding/glm-5.3"}}

	out := ApplySubagentMultiAgent([]*registry.Model{proven, plain}, settings, nil)
	if out[0].MultiAgentVersion != "v1" {
		t.Errorf("disabled model must demote to v1, got %q", out[0].MultiAgentVersion)
	}
	if proven.MultiAgentVersion != "v2" {
		t.Error("original registry object must stay untouched")
	}
	if out[1].MultiAgentVersion != "" {
		t.Errorf("unmarked model must stay unmarked, got %q", out[1].MultiAgentVersion)
	}
	// picker 隐藏同样降权 —— 从 picker 拿掉的模型不该是分身候选。
	hidden := map[string]bool{"zai-coding/glm-5.3": true}
	out = ApplySubagentMultiAgent([]*registry.Model{proven}, state.SubagentSettings{Version: 2, Mode: state.SubagentModeProven}, hidden)
	if out[0].MultiAgentVersion != "v1" {
		t.Error("picker-hidden model must demote to v1")
	}
}

// 本地声明（declared，用户主权通道）：注册表未证明的路由模型可提为
// v2；disabled / 隐藏一票否决永远压过声明。
func TestApplySubagentDeclaredPromotion(t *testing.T) {
	plain := &registry.Model{Slug: "opencode-go/deepseek-v4-flash", Provider: "opencode-go", Listed: true}
	settings := state.SubagentSettings{Version: 2, Mode: state.SubagentModeProven,
		Declared: []string{"opencode-go/deepseek-v4-flash"}}

	out := ApplySubagentMultiAgent([]*registry.Model{plain}, settings, nil)
	if out[0].MultiAgentVersion != "v2" {
		t.Errorf("declared model must promote to v2, got %q", out[0].MultiAgentVersion)
	}
	if plain.MultiAgentVersion != "" {
		t.Error("original registry object must stay untouched")
	}

	// disabled 压过声明。
	settings.Disabled = []string{"opencode-go/deepseek-v4-flash"}
	out = ApplySubagentMultiAgent([]*registry.Model{plain}, settings, nil)
	if out[0].MultiAgentVersion != "v1" {
		t.Error("disabled must override declared promotion")
	}

	// picker 隐藏同样压过声明。
	settings.Disabled = nil
	out = ApplySubagentMultiAgent([]*registry.Model{plain}, settings,
		map[string]bool{"opencode-go/deepseek-v4-flash": true})
	if out[0].MultiAgentVersion != "v1" {
		t.Error("picker-hidden must override declared promotion")
	}
}

// 原生提升：luna 白名单无条件；all 全提；selected 只提加选的；
// disabled 一律不碰。
func TestPromoteNativeMultiAgent(t *testing.T) {
	base := func(slug string) NativeModel {
		return NativeModel{"slug": slug, "visibility": "list", "multi_agent_version": "v1"}
	}
	native := []NativeModel{base("gpt-5.6-sol"), base("gpt-5.6-luna"), base("gpt-5.5")}

	proven := state.SubagentSettings{Version: 2, Mode: state.SubagentModeProven}
	out := PromoteNativeMultiAgent(native, proven)
	if out[1]["multi_agent_version"] != "v2" {
		t.Error("luna whitelist must promote unconditionally")
	}
	if out[0]["multi_agent_version"] != "v1" {
		t.Error("plain native stays v1 in proven mode")
	}

	all := state.SubagentSettings{Version: 2, Mode: state.SubagentModeAll}
	out = PromoteNativeMultiAgent(native, all)
	if out[0]["multi_agent_version"] != "v2" {
		t.Error("all mode promotes every listed native")
	}

	selected := state.SubagentSettings{Version: 2, Mode: state.SubagentModeSelected,
		Enabled: []string{"gpt-5.5"}}
	out = PromoteNativeMultiAgent(native, selected)
	if out[2]["multi_agent_version"] != "v2" || out[0]["multi_agent_version"] != "v1" {
		t.Error("selected promotes only the enabled slug")
	}

	off := state.SubagentSettings{Version: 2, Mode: state.SubagentModeAll,
		Disabled: []string{"gpt-5.5"}}
	out = PromoteNativeMultiAgent(native, off)
	if out[2]["multi_agent_version"] != "v1" {
		t.Error("disabled must never promote")
	}
}

func TestSubagentEligibleModels(t *testing.T) {
	models := []*registry.Model{
		v2Model("zai-coding/glm-5.3"),
		{Slug: "zai-coding/glm-5.2", Provider: "zai-coding", Listed: true},
	}
	settings := state.SubagentSettings{Version: 2, Mode: state.SubagentModeProven,
		Disabled: []string{"zai-coding/glm-5.3"}}
	if eligible := SubagentEligibleModels(models, settings); len(eligible) != 0 {
		t.Errorf("disabled v2 must not be eligible, got %d", len(eligible))
	}
	settings.Disabled = nil
	if eligible := SubagentEligibleModels(models, settings); len(eligible) != 1 || eligible[0].Slug != "zai-coding/glm-5.3" {
		t.Errorf("eligible = %+v", eligible)
	}
}

// RoutedModel 必须把注册表的证明发布进 catalog（v1 默认）。
func TestRoutedModelPublishesMultiAgentVersion(t *testing.T) {
	marked := RoutedModel(NativeModel{}, v2Model("zai-coding/glm-5.3"))
	if marked["multi_agent_version"] != "v2" {
		t.Errorf("v2 declaration must publish, got %v", marked["multi_agent_version"])
	}
	plain := RoutedModel(NativeModel{}, &registry.Model{Slug: "zai-coding/glm-5.2", Listed: true})
	if plain["multi_agent_version"] != "v1" {
		t.Errorf("unmarked must default v1, got %v", plain["multi_agent_version"])
	}
}

// agents 目录同步：合格模型得到定义；不再合格的被删；用户自己的文件
// 永不被碰；model_provider 指向路由器标记块。
func TestSyncRoutedCodexAgents(t *testing.T) {
	dir := t.TempDir()

	first, err := SyncRoutedCodexAgents([]*registry.Model{v2Model("zai-coding/glm-5.3")}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Written) != 1 {
		t.Fatalf("written = %v", first.Written)
	}
	path := filepath.Join(dir, first.Written[0])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents := string(raw)
	if !strings.Contains(contents, `model_provider = "codex-router"`) ||
		!strings.Contains(contents, `model = "zai-coding/glm-5.3"`) ||
		!strings.Contains(contents, `name = "router_zai_coding_glm_5_3"`) {
		t.Errorf("agent definition wrong:\n%s", contents)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("definition mode = %v, want 0600", info.Mode().Perm())
	}

	// 用户自己的文件必须原样存活。
	userFile := filepath.Join(dir, "my-agent.toml")
	os.WriteFile(userFile, []byte("# user's own"), 0o600)

	second, err := SyncRoutedCodexAgents(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Removed) != 1 {
		t.Fatalf("stale definition must be removed, got %v", second.Removed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale definition still present")
	}
	if raw, err := os.ReadFile(userFile); err != nil || string(raw) != "# user's own" {
		t.Error("user's own file must never be touched")
	}
}
