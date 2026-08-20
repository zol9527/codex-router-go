package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tray 的 ProviderUsageSnapshot 解码器（ModelRouterTrayApp.swift）字段
// 大多非可选 —— 这里的断言是「键必须在、类型必须对」，形状漂移会让
// tray 整个用量页渲染失败，而不是少一张卡。
func TestBuildProviderUsageSnapshotShape(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	today := now.Local().Format("2006-01-02")

	events := []Event{
		// 正常计量事件：total 缺失时回落 in+out。
		{At: now.Add(-time.Hour).Format(time.RFC3339), Provider: "zai-coding",
			Model: "zai-coding/glm-5.3", Status: 200, DurationMs: 4_000,
			FirstTokenMs: 1_000, InputTokens: 100, OutputTokens: 50},
		{At: now.Add(-30 * time.Minute).Format(time.RFC3339), Provider: "zai-coding",
			Model: "zai-coding/glm-5.3", Status: 200, DurationMs: 3_000,
			FirstTokenMs: 1_000, InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		// 失败回合计入 requests 不计入 successful。
		{At: now.Add(-20 * time.Minute).Format(time.RFC3339), Provider: "zai-coding",
			Model: "zai-coding/glm-5.3", Status: 500, DurationMs: 500,
			InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		// 无 token 字段（视觉桥转写）不计入。
		{At: now.Add(-10 * time.Minute).Format(time.RFC3339), Provider: "zai-coding",
			Model: "zai-coding/glm-5.3", Status: 200, DurationMs: 900},
		// 原生流量落在 openai 种子上。
		{At: now.Add(-5 * time.Minute).Format(time.RFC3339), Provider: "openai",
			Model: "gpt-5.6-sol", Status: 200, DurationMs: 1_000,
			InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
	}
	rawEvents := make([]byte, 0, 1024)
	for _, e := range events {
		line, _ := json.Marshal(e)
		rawEvents = append(rawEvents, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage-events.jsonl"), rawEvents, 0o600); err != nil {
		t.Fatal(err)
	}

	seeds := []ProviderSeed{
		{ID: "openai", DisplayName: "ChatGPT (native)", CredentialType: "oauth"},
		{ID: "zai-coding", DisplayName: "Z.ai GLM Coding Plan", CredentialType: "api"},
	}
	snapshot := BuildProviderUsageSnapshot(dir, seeds, now)

	if snapshot.Scope != "local-router" {
		t.Errorf("scope = %q", snapshot.Scope)
	}
	if len(snapshot.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(snapshot.Providers))
	}
	if snapshot.Providers[0].ID != "openai" || snapshot.Providers[1].ID != "zai-coding" {
		t.Errorf("seed order not preserved: %s, %s",
			snapshot.Providers[0].ID, snapshot.Providers[1].ID)
	}

	zai := snapshot.Providers[1]
	if zai.Requests != 3 || zai.SuccessfulRequests != 2 || zai.MeteredRequests != 3 {
		t.Errorf("requests=%d successful=%d metered=%d", zai.Requests, zai.SuccessfulRequests, zai.MeteredRequests)
	}
	// total 回落：150 + 30 + 2；in=111, out=71。
	if zai.TotalTokens != 182 || zai.InputTokens != 111 || zai.OutputTokens != 71 {
		t.Errorf("tokens total=%d in=%d out=%d", zai.TotalTokens, zai.InputTokens, zai.OutputTokens)
	}
	if len(zai.DailyUsageBuckets) != 1 ||
		zai.DailyUsageBuckets[0].StartDate != today ||
		zai.DailyUsageBuckets[0].Tokens != 182 ||
		zai.DailyUsageBuckets[0].Requests != 3 {
		t.Errorf("daily buckets = %+v", zai.DailyUsageBuckets)
	}
	if len(zai.Models) != 1 {
		t.Fatalf("models = %d", len(zai.Models))
	}
	if zai.Models[0].DisplayName != "glm-5.3" {
		t.Errorf("model display name = %q, want prefix stripped", zai.Models[0].DisplayName)
	}
	if got := snapshot.Providers[0].Models[0].DisplayName; got != "gpt-5.6-sol" {
		t.Errorf("native display name = %q", got)
	}

	// 速率取首 token 之后的段：事件一 50 tok / 3s ≈ 16.7，事件二 20 tok
	// / 2s = 10 —— Node 中位数语义是 rates[floor(len/2)]，偶数样本取上中位。
	model := zai.Models[0]
	if model.SpeedSampleCount != 2 || model.ObservedTokensPerSecond == nil ||
		*model.ObservedTokensPerSecond < 16.6 || *model.ObservedTokensPerSecond > 16.8 {
		t.Errorf("speed = %+v (%d samples)", model.ObservedTokensPerSecond, model.SpeedSampleCount)
	}
	// TTFT 中位数：两样本 [1000] 与 [1000] → 1000。
	if model.ObservedFirstTokenMs == nil || *model.ObservedFirstTokenMs != 1000 {
		t.Errorf("ttft = %+v", model.ObservedFirstTokenMs)
	}

	// 整树序列化后 tray 非可选键必须在。
	raw, _ := json.Marshal(snapshot)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, ok := generic["fetchedAt"]; !ok {
		t.Error("fetchedAt key missing")
	}
	providers := generic["providers"].([]any)
	for _, p := range providers {
		row := p.(map[string]any)
		for _, key := range []string{"id", "displayName", "credentialType", "scope",
			"requests", "successfulRequests", "meteredRequests",
			"inputTokens", "outputTokens", "totalTokens",
			"dailyUsageBuckets", "models", "account"} {
			if _, ok := row[key]; !ok {
				t.Errorf("provider %v missing key %q", row["id"], key)
			}
		}
		for _, m := range row["models"].([]any) {
			for _, key := range []string{"slug", "displayName", "requests",
				"successfulRequests", "meteredRequests", "inputTokens",
				"outputTokens", "totalTokens", "speedSampleCount",
				"observedTokensPerSecond", "lastUsedAt"} {
				if _, ok := m.(map[string]any)[key]; !ok {
					t.Errorf("model %v missing key %q", m.(map[string]any)["slug"], key)
				}
			}
		}
	}
}

// 不可信速率（检测失败的流）与空补全回合不进速率样本。
func TestSpeedSampleFiltering(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	events := []Event{
		// 5000 tok / 10ms = 500,000 tok/s —— 远超可信上限，丢弃。
		{At: now.Add(-time.Hour).Format(time.RFC3339), Provider: "zai-coding",
			Model: "m", Status: 200, DurationMs: 10, FirstTokenMs: 1,
			OutputTokens: 5000, InputTokens: 1},
		// 空补全回合不可计量。
		{At: now.Add(-time.Hour).Format(time.RFC3339), Provider: "zai-coding",
			Model: "m", Status: 200, DurationMs: 2_000, FirstTokenMs: 500,
			EmptyCompletion: true, OutputTokens: 100, InputTokens: 1},
	}
	rawEvents := make([]byte, 0, 512)
	for _, e := range events {
		line, _ := json.Marshal(e)
		rawEvents = append(rawEvents, append(line, '\n')...)
	}
	os.WriteFile(filepath.Join(dir, "usage-events.jsonl"), rawEvents, 0o600)

	snapshot := BuildProviderUsageSnapshot(dir,
		[]ProviderSeed{{ID: "zai-coding", DisplayName: "Z.ai", CredentialType: "api"}}, now)
	if len(snapshot.Providers[0].Models) != 1 {
		t.Fatal("model row missing")
	}
	m := snapshot.Providers[0].Models[0]
	if m.SpeedSampleCount != 0 || m.ObservedTokensPerSecond != nil {
		t.Errorf("untrusted samples leaked: count=%d rate=%v", m.SpeedSampleCount, m.ObservedTokensPerSecond)
	}
}

// CodexAccountUsage 的 summary 键非可选：缺键让 tray 整个账号卡解码失败。
func TestCodexAccountUsageAlwaysCarriesSummary(t *testing.T) {
	usage := normalizeCodexUsage(
		[]byte(`{"rateLimits":{"planType":"plus","primary":{"usedPercent":35,"windowDurationMins":10080,"resetsAt":1787197067}}}`),
		[]byte(`{"summary":{"lifetimeTokens":1997211},"dailyUsageBuckets":[{"startDate":"2025-08-26","tokens":123}]}`),
	)
	raw, _ := json.Marshal(usage)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	summary, ok := generic["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary key missing or not an object: %s", raw)
	}
	if summary["lifetimeTokens"] != float64(1997211) {
		t.Errorf("lifetimeTokens = %v", summary["lifetimeTokens"])
	}
	// 上游没给的字段以 null 呈现，不是 0。
	if _, present := summary["peakDailyTokens"]; !present {
		t.Error("peakDailyTokens key must exist (as null)")
	}
}
