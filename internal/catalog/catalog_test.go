package catalog

import (
	"testing"

	"github.com/loyd/codex-router/internal/registry"
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
