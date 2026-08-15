package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/loyd/codex-router/internal/modelmeta"
	"github.com/loyd/codex-router/internal/registry"
)

func testProvider() *registry.Provider {
	return &registry.Provider{
		ID: "zai-coding", DisplayName: "Z.ai", Kind: "openai-compatible",
		BaseURL: "https://api.z.ai/api/coding/paas/v4", ModelsDevID: "zai-coding-plan",
	}
}

func stubShelf(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		payload := map[string]any{"data": []any{}}
		data := payload["data"].([]any)
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchModelsDedupAndAuth(t *testing.T) {
	server := stubShelf(t, "glm-5.4", "GLM-5.4", "glm-5.3:free")
	models, err := FetchModels(context.Background(), server.Client(), server.URL, "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("case/alias dedup failed: %v", models)
	}
	if _, err := FetchModels(context.Background(), server.Client(), server.URL, "wrong"); err == nil {
		t.Fatal("rejected credential must error")
	}
}

// Sync 全链路：货架有新模型（models.dev 命中→精确参数；未命中→克隆
// 兜底）；已收录的不动；覆盖层落盘且撞内嵌的不重复注册。
func TestSyncRegistersNewModels(t *testing.T) {
	dir := t.TempDir()
	// models.dev 桩数据库。
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"zai-coding-plan": map[string]any{
				"models": map[string]any{
					"glm-5.4": map[string]any{
						"name": "GLM-5.4", "description": "new flagship",
						"limit": map[string]int{"context": 1048576, "output": 131072},
						"reasoning": true, "tool_call": true,
						"modalities": map[string][]string{"input": {"text", "image"}},
					},
				},
			},
		})
	}))
	defer metaServer.Close()
	modelmeta.DefaultSource = modelmeta.Source{BaseURL: metaServer.URL}
	defer func() { modelmeta.DefaultSource = modelmeta.Source{} }()

	// 货架桩：glm-5.3（已收录）、glm-5.4（models.dev 命中）、glm-4.9-lite（未命中→克隆）。
	shelf := stubShelf(t, "glm-5.3", "glm-5.4", "glm-4.9-lite")

	// 注册表：内嵌一个 glm-5.3（作为克隆源与已收录项）。
	reg := registry.FromDefinitions([]registry.Provider{*testProvider()}, []registry.Model{{
		Slug: "zai-coding/glm-5.3", GatewayModel: "zai-coding-glm-5-3",
		UpstreamModel: "glm-5.3", Provider: "zai-coding", Listed: true,
		DisplayName: "GLM-5.3", ContextWindow: 1048576,
		DefaultEffort: "max",
		ReasoningLevels: []registry.ReasoningLevel{{Effort: "max", Description: "deep"}},
	}})
	p := reg.Providers["zai-coding"]
	p.BaseURL = shelf.URL

	report := Sync(context.Background(), dir, reg, "zai-coding", "sk-test")
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(report.Added) != 2 {
		t.Fatalf("added = %d (want 2): %+v", len(report.Added), report)
	}
	if report.MetaHits != 1 || report.Clones != 1 {
		t.Fatalf("meta=%d clones=%d, want 1/1", report.MetaHits, report.Clones)
	}
	if len(report.Skipped) != 1 || report.Skipped[0] != "glm-5.3" {
		t.Fatalf("skipped = %v", report.Skipped)
	}

	bySlug := map[string]registry.UserModelEntry{}
	for _, e := range registry.ReadUserModels(dir) {
		bySlug[e.Model.Slug] = e
	}
	// models.dev 命中：精确参数（1M ctx、视觉模态、描述）。
	meta := bySlug["zai-coding/glm-5.4"]
	if meta.Source != "modelsdev" || meta.Model.ContextWindow != 1048576 {
		t.Fatalf("meta entry wrong: %+v", meta)
	}
	if len(meta.Model.InputModalities) != 2 { // text+image = 视觉
		t.Errorf("modalities = %v, want text+image", meta.Model.InputModalities)
	}
	if meta.Model.UpstreamModel != "glm-5.4" || meta.Model.Provider != "zai-coding" {
		t.Errorf("identity fields wrong: %+v", meta.Model)
	}
	// 克隆兜底：effort 档位来自家族。
	clone := bySlug["zai-coding/glm-4.9-lite"]
	if clone.Source != "clone" {
		t.Fatalf("clone entry source = %q", clone.Source)
	}
	if len(clone.Model.ReasoningLevels) != 1 || clone.Model.ReasoningLevels[0].Effort != "max" {
		t.Errorf("clone must inherit family effort levels: %+v", clone.Model.ReasoningLevels)
	}
	if clone.Model.ContextWindow != 1048576 {
		t.Errorf("clone context should come from family: %d", clone.Model.ContextWindow)
	}

	// 覆盖层文件权限。
	info, err := os.Stat(filepath.Join(dir, "user-models.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("overlay perms: %v %v", err, info)
	}

	// 幂等：再跑一遍不再新增。
	report = Sync(context.Background(), dir, reg, "zai-coding", "sk-test")
	if report.Err != nil || len(report.Added) != 0 {
		t.Fatalf("second sync must add nothing: %+v", report)
	}
}

// 撞车红线：覆盖层不得覆盖内嵌注册表（升级二进制的正式收录永远赢）。
func TestOverlayNeverOverridesEmbedded(t *testing.T) {
	entries := []registry.UserModelEntry{{
		Model: registry.Model{
			Slug: "zai-coding/glm-5.3", GatewayModel: "zai-coding-glm-5-3",
			UpstreamModel: "conflicting", Provider: "zai-coding",
		},
	}}
	reg := registry.FromDefinitions([]registry.Provider{*testProvider()}, []registry.Model{{
		Slug: "zai-coding/glm-5.3", GatewayModel: "zai-coding-glm-5-3",
		UpstreamModel: "glm-5.3", Provider: "zai-coding", Listed: true,
	}})
	merged, skipped := registry.ApplyOverlay(reg, entries)
	if len(skipped) != 1 {
		t.Fatalf("collision must be skipped, got %v", skipped)
	}
	if merged.BySlug("zai-coding/glm-5.3").UpstreamModel != "glm-5.3" {
		t.Fatal("embedded entry must win over overlay")
	}
}
