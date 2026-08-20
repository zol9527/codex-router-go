package controlplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// Snapshot 的 JSON 形状是 Swift RouterSnapshot 解码器的契约。
// 这些测试钉住历史上出过事故的契约点：
//   - providers/models 空时必须是 []，null 会让 Swift [T] 解码失败；
//   - enabledProviders / providers / models 字段名不可漂移；
//   - multiAgentVersion 仅在 v2 裁决后出现（omitempty）；
//   - presence 子块原样透传。
func TestBuildSnapshotJSONShape(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := BuildSnapshot(SnapshotDeps{
		State:  st,
		Reg:    &registry.Registry{Providers: map[string]*registry.Provider{}, Models: []*registry.Model{}},
		Version: "test-version",
		Active: false,
		Presence:     json.RawMessage(`{"mode":"follow-codex","effectiveMode":"follow-codex","harnessPublished":false,"terminalCodex":false}`),
		Subagents:    json.RawMessage(`{"mode":"proven","enabled":[],"disabled":[],"all":false}`),
		Picker:       json.RawMessage(`{"hidden":[]}`),
		VisionBridge: json.RawMessage(`{"enabled":false}`),
	})
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(raw)

	for _, field := range []string{
		`"targets":{"codex":{`,
		`"target":"codex"`,
		`"configured":true`,
		`"active":false`,
		`"enabledProviders":[]`,
		`"providers":[]`,
		`"models":[]`,
		`"modelSettings":{"subagents":{"mode":"proven"`,
		`"version":"test-version"`,
		`"presence":{"mode":"follow-codex","effectiveMode":"follow-codex","harnessPublished":false,"terminalCodex":false}`,
	} {
		if !strings.Contains(payload, field) {
			t.Errorf("snapshot JSON missing contract fragment %q\npayload: %s", field, payload)
		}
	}
	if strings.Contains(payload, "null") {
		t.Errorf("snapshot JSON must not contain null (Swift [T] decode fails)\npayload: %s", payload)
	}
	if strings.Contains(payload, "multiAgentVersion") {
		t.Errorf("multiAgentVersion must be omitted without a v2 resolution\npayload: %s", payload)
	}
}

// 有模型时 multiAgentVersion 只在分身裁决为 v2 时出现；proven 跟随
// 注册表证明，declared 跟随本地声明。
func TestBuildSnapshotModelEntries(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabledProviders([]string{"test"}); err != nil {
		t.Fatal(err)
	}
	reg := &registry.Registry{
		Providers: map[string]*registry.Provider{
			"test": {ID: "test", DisplayName: "Test", Kind: "openai-compatible"},
		},
		Models: []*registry.Model{
			{Slug: "test/proven-v2", DisplayName: "P", Provider: "test", Listed: true, MultiAgentVersion: "v2"},
			{Slug: "test/plain", DisplayName: "Q", Provider: "test", Listed: true},
			{Slug: "test/unlisted", DisplayName: "H", Provider: "test", Listed: false},
		},
	}
	snapshot := BuildSnapshot(SnapshotDeps{
		State: st, Reg: reg, Version: "t",
		Presence: json.RawMessage(`{}`), Subagents: json.RawMessage(`{}`),
		Picker: json.RawMessage(`{}`), VisionBridge: json.RawMessage(`{}`),
	})
	target := snapshot.Targets["codex"]
	if len(target.Models) != 2 {
		t.Fatalf("models = %d, want 2 (unlisted excluded)", len(target.Models))
	}
	bySlug := map[string]ModelInfo{}
	for _, m := range target.Models {
		bySlug[m.Slug] = m
	}
	if m := bySlug["test/proven-v2"]; !m.Proven || m.MultiAgentVersion != "v2" || !m.Enabled || !m.Visible {
		t.Errorf("proven v2 entry wrong: %+v", m)
	}
	if m := bySlug["test/plain"]; m.Proven || m.MultiAgentVersion != "" || m.Declared {
		t.Errorf("plain entry wrong: %+v", m)
	}
	if len(target.Providers) != 1 || target.Providers[0].ID != "test" {
		t.Errorf("providers = %+v", target.Providers)
	}
}

// 注册表携带 models.dev 生态的参考 provider 定义；未启用的白名单外
// provider 不得涌入设置页（2026-08-20 部署实测曾 3 → 30 膨胀），
// 用户显式启用的才追加展示。
func TestOrderedProvidersLimitsToSupportedOrEnabled(t *testing.T) {
	reg := &registry.Registry{Providers: map[string]*registry.Provider{
		"zai-coding": {ID: "zai-coding"},
		"litellm":    {ID: "litellm"},
		"zai-api":    {ID: "zai-api"},
		"deepseek":   {ID: "deepseek"},
		"grok-api":   {ID: "grok-api"},
	}}
	enabled := func(id string) bool { return id == "zai-api" }

	ids := func(list []*registry.Provider) []string {
		out := []string{}
		for _, p := range list {
			out = append(out, p.ID)
		}
		return out
	}

	// 未启用任何白名单外 provider：只有支持面（zai-coding、litellm；
	// opencode-go 未注册自动缺席）。
	got := ids(OrderedProviders(reg, func(string) bool { return false }))
	want := []string{"zai-coding", "litellm"}
	if len(got) != len(want) {
		t.Fatalf("no-extra-enabled providers = %v, want %v", got, want)
	}
	// 启用 zai-api：追加在支持面之后；deepseek/grok-api 仍被排除。
	got = ids(OrderedProviders(reg, enabled))
	want = []string{"zai-coding", "litellm", "zai-api"}
	if len(got) != len(want) {
		t.Fatalf("enabled-append providers = %v, want %v", got, want)
	}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("enabled-append order = %v, want %v", got, want)
		}
	}
}
