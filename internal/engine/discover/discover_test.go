package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/lib/modelmeta"
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
	if models[0].ID != "glm-5.3:free" || models[1].ID != "glm-5.4" {
		t.Fatalf("unexpected ids: %+v", models)
	}
	if _, err := FetchModels(context.Background(), server.Client(), server.URL, "wrong"); err == nil {
		t.Fatal("rejected credential must error")
	}
}

// TestFetchModelsParsesSelfReport：LiteLLM 的 model_info 体系把部署级
// 窗口随 /v1/models 走（max_input_tokens/max_output_tokens），货架解析
// 必须带出来 —— 这是唯一优于本地猜测链的真值渠道（2026-08-20 实证：
// volcengine/deepseek-v4-flash 自报 1M，猜测链兜到 128k）。
func TestFetchModelsParsesSelfReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "volcengine/deepseek-v4-flash",
				"max_input_tokens": 1000000, "max_output_tokens": 393216},
			map[string]any{"id": "plain-model"}, // 无自报字段的条目照常工作
		}})
	}))
	defer server.Close()
	models, err := FetchModels(context.Background(), server.Client(), server.URL, "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ID != "plain-model" || models[0].MaxInputTokens != 0 || models[0].MaxOutputTokens != 0 {
		t.Errorf("plain entry polluted: %+v", models[0])
	}
	deep := models[1]
	if deep.ID != "volcengine/deepseek-v4-flash" ||
		deep.MaxInputTokens != 1000000 || deep.MaxOutputTokens != 393216 {
		t.Fatalf("self-report fields lost: %+v", deep)
	}
}

// TestBuildEntrySelfReportWins：上游自报窗口压过克隆与 provider 默认
// （部署级真值优先于本地猜测），autoCompact 取 90%；无自报时原链路不变。
func TestBuildEntrySelfReportWins(t *testing.T) {
	cloneSrc := &registry.Model{
		Slug: "zai-coding/glm-5-turbo", UpstreamModel: "glm-5-turbo", Provider: "zai-coding",
		ContextWindow:   131072,
		ReasoningLevels: []registry.ReasoningLevel{{Effort: "low", Description: "low"}},
	}
	// 不带 ModelsDevID：隔离 models.dev，只验 self 与克隆/默认的优先级。
	p := &registry.Provider{ID: "zai-coding"}

	entry := buildEntry(p, "glm-4.5-air", cloneSrc,
		&UpstreamModel{ID: "glm-4.5-air", MaxInputTokens: 1000000},
		ModelOverrides{}, t.TempDir(), context.Background())
	if entry.Model.ContextWindow != 1000000 || entry.Model.AutoCompact != 900000 {
		t.Fatalf("self-report must override clone: ctx=%d compact=%d",
			entry.Model.ContextWindow, entry.Model.AutoCompact)
	}

	// 无自报：provider 默认分支 + self=nil，行为与旧链路一致
	//（ctx 落默认值，effort 兜底单一 medium）。
	def := &registry.Provider{ID: "litellm", DefaultContextWindow: 131072}
	entry = buildEntry(def, "volcengine/deepseek-v4-flash", nil, nil, ModelOverrides{}, t.TempDir(), context.Background())
	if entry.Model.ContextWindow != 131072 || entry.Model.AutoCompact != 117964 {
		t.Fatalf("nil self must keep provider default: %+v", entry.Model)
	}
	if entry.Model.DefaultEffort != "medium" || len(entry.Model.ReasoningLevels) != 1 {
		t.Fatalf("effort fallback must hold without self-report: %+v", entry.Model)
	}
}

// TestSyncUsesSelfReport：货架自报元数据要流过 Sync 全链路落进覆盖层。
func TestSyncUsesSelfReport(t *testing.T) {
	dir := t.TempDir()
	// models.dev 桩：空库（未命中 → 走克隆链）。
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer metaServer.Close()
	modelmeta.DefaultSource = modelmeta.Source{BaseURL: metaServer.URL}
	defer func() { modelmeta.DefaultSource = modelmeta.Source{} }()

	shelf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": "glm-4.9-lite", "max_input_tokens": 200000},
		}})
	}))
	defer shelf.Close()

	reg := registry.FromDefinitions([]registry.Provider{*testProvider()}, []registry.Model{{
		Slug: "zai-coding/glm-5.3", GatewayModel: "zai-coding-glm-5-3",
		UpstreamModel: "glm-5.3", Provider: "zai-coding", Listed: true,
		DisplayName: "GLM-5.3", ContextWindow: 1048576,
	}})
	p := reg.Providers["zai-coding"]
	p.BaseURL = shelf.URL

	report := Sync(context.Background(), dir, reg, "zai-coding", "sk-test")
	if report.Err != nil || len(report.Added) != 1 {
		t.Fatalf("sync report: %+v", report)
	}
	entry := report.Added[0]
	if entry.Model.ContextWindow != 200000 || entry.Model.AutoCompact != 180000 {
		t.Fatalf("self-report must flow through Sync: ctx=%d compact=%d",
			entry.Model.ContextWindow, entry.Model.AutoCompact)
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
						"limit":     map[string]int{"context": 1048576, "output": 131072},
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
		DefaultEffort:   "max",
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

// TestCloneEntryDisplayNameDerivedFromUpstreamID 钉死显示名红线：
// 克隆兜底条目的显示名必须由上游 ID 派生，绝不继承克隆源全名，
// 否则同家族多个模型在选择表里显示成同一个名字，看起来像重复。
func TestCloneEntryDisplayNameDerivedFromUpstreamID(t *testing.T) {
	cloneSrc := &registry.Model{
		Slug: "zai-coding/glm-5-turbo", GatewayModel: "zai-coding-glm-5-turbo",
		UpstreamModel: "glm-5-turbo", DisplayName: "GLM-5-Turbo (Coding Plan)",
		ContextWindow: 131072, ReasoningLevels: []registry.ReasoningLevel{{Effort: "low", Description: "low"}},
	}
	p := &registry.Provider{ID: "zai-coding"}
	entry := buildEntry(p, "glm-4.5-air", cloneSrc, nil, ModelOverrides{}, t.TempDir(), context.Background())
	if entry.Model.DisplayName == cloneSrc.DisplayName {
		t.Fatalf("clone inherited source display name %q", entry.Model.DisplayName)
	}
	if entry.Model.DisplayName != "GLM-4.5-Air (Coding Plan)" {
		t.Fatalf("unexpected derived display name %q", entry.Model.DisplayName)
	}
	if entry.Model.Slug != "zai-coding/glm-4.5-air" {
		t.Fatalf("unexpected slug %q", entry.Model.Slug)
	}
}

// TestBuildEntryOverridesWin：用户主权声明压过 self-report/克隆/默认
// 整条猜测链（--efforts/--default-effort/--context-window）；零值字段
// 不覆盖对应来源。
func TestBuildEntryOverridesWin(t *testing.T) {
	cloneSrc := &registry.Model{
		Slug: "zai-coding/glm-5-turbo", UpstreamModel: "glm-5-turbo", Provider: "zai-coding",
		ContextWindow:   131072,
		ReasoningLevels: []registry.ReasoningLevel{{Effort: "low", Description: "low"}},
	}
	p := &registry.Provider{ID: "zai-coding"}
	self := &UpstreamModel{ID: "glm-4.5-air", MaxInputTokens: 200000}

	entry := buildEntry(p, "glm-4.5-air", cloneSrc, self, ModelOverrides{
		Efforts:       []string{"minimal", "high"},
		ContextWindow: 1048576,
	}, t.TempDir(), context.Background())
	if entry.Model.ContextWindow != 1048576 || entry.Model.AutoCompact != 943718 {
		t.Fatalf("override ctx must win: ctx=%d compact=%d", entry.Model.ContextWindow, entry.Model.AutoCompact)
	}
	if len(entry.Model.ReasoningLevels) != 2 ||
		entry.Model.ReasoningLevels[0].Effort != "minimal" || entry.Model.ReasoningLevels[1].Effort != "high" {
		t.Fatalf("override efforts must win: %+v", entry.Model.ReasoningLevels)
	}
	// --efforts 声明而 --default-effort 缺省：默认档取首档。
	if entry.Model.DefaultEffort != "minimal" {
		t.Fatalf("default must fall to first effort: %q", entry.Model.DefaultEffort)
	}

	// 显式 default-effort 覆盖；零值 ContextWindow 不动 self-report 的窗口。
	entry = buildEntry(p, "glm-4.5-air", cloneSrc, self, ModelOverrides{
		DefaultEffort: "high",
	}, t.TempDir(), context.Background())
	if entry.Model.ContextWindow != 200000 {
		t.Fatalf("nil ctx override must keep self-report: %d", entry.Model.ContextWindow)
	}
	if entry.Model.DefaultEffort != "high" {
		t.Fatalf("explicit default must win: %q", entry.Model.DefaultEffort)
	}
}
