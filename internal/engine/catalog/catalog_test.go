package catalog

import (
	"testing"

	"github.com/loyd/codex-router/internal/domain/registry"
)

// 反馈环红线：路由器发布进 Codex 的路由模型会从 codex debug models
// "还魂"回原生列表 —— Build 必须把它们过滤掉，否则 picker 里每个
// 路由模型出现两份（实测事故：v2/null 的反馈份 + v1/false 的注册表份）。
func TestBuildFiltersFedBackRoutedEntries(t *testing.T) {
	routed := []*registry.Model{{
		Slug: "zai-coding/glm-5.3", GatewayModel: "zai-coding-glm-5-3",
		UpstreamModel: "glm-5.3", Provider: "zai-coding", Listed: true,
	}}
	native := []NativeModel{
		{"slug": "gpt-5.6-sol", "visibility": "list", "multi_agent_version": "v2"},
		// 反馈份：路由 slug 原样出现在"原生"列表里（旧发布还带僵尸条目）。
		{"slug": "zai-coding/glm-5.3", "visibility": "list", "multi_agent_version": "v2"},
		{"slug": "zai-coding/glm-5.3-1m", "visibility": "list"},
		{"slug": "zai-coding-glm-5-3", "visibility": "list"}, // gateway 形态的反馈
	}
	catalog := Build(native, routed, func(string) bool { return true }, true, nil)
	slugs := map[string]int{}
	for _, m := range catalog["models"].([]NativeModel) {
		slug, _ := m["slug"].(string)
		slugs[slug]++
	}
	if slugs["zai-coding/glm-5.3"] != 1 {
		t.Errorf("routed slug must appear exactly once, got %d", slugs["zai-coding/glm-5.3"])
	}
	if _, ok := slugs["zai-coding/glm-5.3-1m"]; ok {
		t.Error("zombie routed entry must be filtered out of native list")
	}
	if _, ok := slugs["zai-coding-glm-5-3"]; ok {
		t.Error("gateway-form fed-back entry must be filtered")
	}
	if slugs["gpt-5.6-sol"] != 1 {
		t.Error("real native entry must survive")
	}
}

// 克隆 upstream 撞原生 slug（2026-08-15 实发事故）：models.dev 把原生
// 模型挂到网关下，动态发现生成克隆条目，其 upstream_model 就是原生
// slug。upstream 不得当反馈键，否则真原生 gpt-5.6-luna 被误杀、picker
// 里凭空消失。克隆自身的反馈份（带 "/"）仍须过滤。
func TestBuildKeepsNativeWhenCloneUpstreamCollides(t *testing.T) {
	routed := []*registry.Model{{
		Slug: "opencode-go-responses/gpt-5.6-luna", GatewayModel: "opencode-go-gpt-5-6-luna",
		UpstreamModel: "gpt-5.6-luna", Provider: "opencode-go", Listed: true,
	}}
	native := []NativeModel{
		{"slug": "gpt-5.6-luna", "visibility": "list"},
		{"slug": "gpt-5.6-sol", "visibility": "list"},
		// 克隆的反馈份（路由 slug 原样出现在"原生"列表里）。
		{"slug": "opencode-go-responses/gpt-5.6-luna", "visibility": "list"},
	}
	catalog := Build(native, routed, func(string) bool { return true }, true, nil)
	slugs := map[string]int{}
	for _, m := range catalog["models"].([]NativeModel) {
		slug, _ := m["slug"].(string)
		slugs[slug]++
	}
	if slugs["gpt-5.6-luna"] != 1 {
		t.Errorf("native entry whose slug collides with a clone upstream must survive, got %d", slugs["gpt-5.6-luna"])
	}
	if slugs["gpt-5.6-sol"] != 1 {
		t.Error("untouched native entry must survive")
	}
	if slugs["opencode-go-responses/gpt-5.6-luna"] != 1 {
		t.Errorf("clone must appear exactly once (registry copy only), got %d", slugs["opencode-go-responses/gpt-5.6-luna"])
	}
}

// TestRoutedModelExpandsReasoningLevels：发布集必须区间展开 —— 真实
// 档位保留原名，Codex 词汇档位（low/medium/high/xhigh）全部可请求
// （2026-08-20 explorer spawn 三连拒的根因修复：Codex 校验发生在
// 请求到达 router 之前）。空档位兜底的单一 medium 同样展开，未知
// 模型从此也不会被词汇差异拒掉。
func TestRoutedModelExpandsReasoningLevels(t *testing.T) {
	collect := func(m *registry.Model) map[string]string {
		out := map[string]string{}
		for _, raw := range RoutedModel(NativeModel{}, m)["supported_reasoning_levels"].([]any) {
			entry := raw.(map[string]any)
			out[entry["effort"].(string)], _ = entry["description"].(string)
		}
		return out
	}

	// deepseek 形状 [minimal, high]：Codex 四词汇全部可请求，
	// 映射描述指向区间归属的真实档。
	levels := collect(&registry.Model{
		Slug: "litellm/deepseek", ReasoningLevels: []registry.ReasoningLevel{
			{Effort: "minimal"}, {Effort: "high"},
		}, DefaultEffort: "high",
	})
	for _, rung := range []string{"minimal", "high", "low", "medium", "xhigh"} {
		if _, ok := levels[rung]; !ok {
			t.Fatalf("rung %q missing from expanded set: %v", rung, levels)
		}
	}
	if levels["medium"] != "Reasoning effort (maps to high)" {
		t.Errorf("medium should map to high, got %q", levels["medium"])
	}
	if levels["low"] != "Reasoning effort (maps to minimal)" {
		t.Errorf("low should map to minimal, got %q", levels["low"])
	}

	// GLM 形状 [low, high, max]：真实档位原名保留，medium 补进（平手 → low）。
	levels = collect(&registry.Model{
		Slug: "zai-coding/glm-5.3", ReasoningLevels: []registry.ReasoningLevel{
			{Effort: "low"}, {Effort: "high"}, {Effort: "max"},
		}, DefaultEffort: "max",
	})
	for _, rung := range []string{"low", "high", "max", "medium", "xhigh"} {
		if _, ok := levels[rung]; !ok {
			t.Fatalf("glm rung %q missing: %v", rung, levels)
		}
	}

	// 空档位兜底（单一 medium）：Codex 词汇同样全部可请求（映射到 medium）。
	levels = collect(&registry.Model{Slug: "litellm/unknown"})
	for _, rung := range []string{"medium", "low", "high", "xhigh"} {
		if _, ok := levels[rung]; !ok {
			t.Fatalf("fallback rung %q missing: %v", rung, levels)
		}
	}
}
