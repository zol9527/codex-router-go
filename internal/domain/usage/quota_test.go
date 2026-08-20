package usage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// opencode 窗口：rolling/weekly/monthly 百分比 + reset。
func TestOpencodeGoUsageMetrics(t *testing.T) {
	payload := map[string]any{
		"usage": map[string]any{
			"rolling": map[string]any{"percent": 42.5, "resetsAt": 1786732800000.0},
			"weekly":  map[string]any{"percent": 10.0},
			"monthly": map[string]any{"percent": 3.0},
		},
	}
	metrics := OpencodeGoUsageMetrics(payload)
	if len(metrics) != 3 {
		t.Fatalf("metrics = %d, want 3", len(metrics))
	}
	if metrics[0].Label != "Rolling limit" || metrics[0].UsedPercent != 42.5 ||
		metrics[0].RemainingPercent != 57.5 {
		t.Errorf("rolling wrong: %+v", metrics[0])
	}
	if metrics[0].ResetAt == nil || *metrics[0].ResetAt != 1786732800000 {
		t.Errorf("rolling reset missing: %+v", metrics[0])
	}
	// usage 缺失 → 空。
	if got := OpencodeGoUsageMetrics(map[string]any{}); got != nil {
		t.Errorf("no usage block = nil, got %v", got)
	}
	// 越界钳制。
	extreme := OpencodeGoUsageMetrics(map[string]any{
		"usage": map[string]any{"rolling": map[string]any{"percent": 150.0}},
	})
	if extreme[0].UsedPercent != 100 {
		t.Errorf("over-100 must clamp: %+v", extreme[0])
	}
}

// zai 配额：计数器优先于（陈旧的）百分比、TOKENS_LIMIT 标签、
// MCP 月度标记跳过、reset 毫秒→秒。
func TestZAIQuotaMetrics(t *testing.T) {
	data := map[string]any{
		"limits": []any{
			// 计数器形态（30/100 → 30%），即使 percentage 声称 0。
			map[string]any{"type": "TIME_LIMIT", "unit": 1.0, "number": 1.0,
				"usage": 100.0, "currentValue": 30.0, "percentage": 0.0,
				"nextResetTime": 1786732800000.0},
			// TOKENS_LIMIT → "Daily tokens"。
			map[string]any{"type": "TOKENS_LIMIT", "unit": 1.0, "number": 1.0,
				"usage": 1000.0, "currentValue": 250.0},
			// MCP 月度标记：跳过。
			map[string]any{"type": "TIME_LIMIT", "unit": 5.0, "number": 1.0, "percentage": 50.0},
			// 只有百分比可用时的回退。
			map[string]any{"type": "TIME_LIMIT", "unit": 6.0, "number": 1.0, "percentage": 80.0},
		},
	}
	metrics := ZAIQuotaMetrics(data)
	if len(metrics) != 3 {
		t.Fatalf("metrics = %d (MCP marker must be skipped), got %+v", len(metrics), metrics)
	}
	if metrics[0].Label != "Daily limit" || metrics[0].UsedPercent != 30 {
		t.Errorf("counter must win over percentage: %+v", metrics[0])
	}
	if metrics[0].ResetAt == nil || *metrics[0].ResetAt != 1786732800 {
		t.Errorf("reset must be ms→s: %+v", metrics[0])
	}
	if metrics[1].Label != "Daily tokens" {
		t.Errorf("TOKENS_LIMIT label = %q", metrics[1].Label)
	}
	if metrics[2].Label != "Weekly limit" || metrics[2].UsedPercent != 80 {
		t.Errorf("percentage fallback wrong: %+v", metrics[2])
	}
}

// 账号拉取端到端：zai 成功形态（success/code 门 + planName）与错误形态。
func TestFetchZAI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true, "code": 200,
			"data": map[string]any{
				"planName": "GLM Coding Pro",
				"limits": []any{map[string]any{
					"type": "TIME_LIMIT", "unit": 1.0, "number": 1.0,
					"usage": 100.0, "currentValue": 60.0,
				}},
			},
		})
	}))
	defer upstream.Close()
	setZAIQuotaURL(upstream.URL)

	snapshot := FetchAccount(context.Background(), nil, "zai-coding", "test-key")
	if snapshot.Status != "available" || snapshot.Plan != "GLM Coding Pro" {
		t.Fatalf("snapshot wrong: %+v", snapshot)
	}
	if len(snapshot.Metrics) != 1 || snapshot.Metrics[0].UsedPercent != 60 {
		t.Errorf("metrics wrong: %+v", snapshot.Metrics)
	}
	if snapshot.DashboardURL == "" {
		t.Error("dashboard URL missing")
	}

	// 未配置。
	if snapshot := FetchAccount(context.Background(), nil, "zai-coding", ""); snapshot.Status != "not-configured" {
		t.Errorf("empty credential = not-configured, got %v", snapshot.Status)
	}
}

// opencode 账号端到端。
func TestFetchOpencode(t *testing.T) {
	snapshot := FetchAccount(context.Background(), &http.Client{
		Transport: fakeTransport{response: `{"usage":{"rolling":{"percent":50}}}`},
	}, "opencode-go", "key")
	if snapshot.Status != "available" || len(snapshot.Metrics) != 1 {
		t.Fatalf("snapshot wrong: %+v", snapshot)
	}
	if snapshot.Metrics[0].UsedPercent != 50 {
		t.Errorf("metric wrong: %+v", snapshot.Metrics[0])
	}
}

type fakeTransport struct{ response string }

func (f fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       jsonBody(f.response),
		Header:     http.Header{},
	}, nil
}

// ChatGPT 窗口归一：百分比钳制、日桶过滤排序。
func TestNormalizeCodexUsage(t *testing.T) {
	rateLimits := json.RawMessage(`{"rateLimits":{"planType":"pro","primary":{"usedPercent":35.5,"windowDurationMins":300},"secondary":{"usedPercent":-5}}}`)
	usagePayload := json.RawMessage(`{"dailyUsageBuckets":[{"startDate":"2026-08-14","tokens":1200},{"startDate":"bad"},{"startDate":"2026-08-13","tokens":800}],"summary":{"lifetimeTokens":9}}`)
	account := normalizeCodexUsage(rateLimits, usagePayload)
	if account.PlanType != "pro" {
		t.Errorf("plan = %q", account.PlanType)
	}
	if account.Primary == nil || account.Primary.UsedPercent != 35.5 || account.Primary.RemainingPercent != 64.5 {
		t.Errorf("primary window wrong: %+v", account.Primary)
	}
	if account.Secondary == nil || account.Secondary.UsedPercent != 0 {
		t.Errorf("negative must clamp to 0: %+v", account.Secondary)
	}
	if len(account.Buckets) != 2 {
		t.Fatalf("buckets = %d (bad date filtered)", len(account.Buckets))
	}
}

func jsonBody(payload string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(payload))
}
