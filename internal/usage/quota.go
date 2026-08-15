package usage

// 配额卡片，移植 provider-account-usage.mjs 的 zai/opencode 部分与
// codex-account-usage.mjs：tray 的 "% left" 显示与用量卡片数据源。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Metric 是一个配额/余额指标（tray 卡片的一个条目）。
type Metric struct {
	Kind             string   `json:"kind"` // "quota" | "balance"
	Label            string   `json:"label"`
	UsedPercent      float64  `json:"usedPercent"`
	RemainingPercent float64  `json:"remainingPercent"`
	Used             float64  `json:"used"`
	Limit            float64  `json:"limit"`
	Remaining        float64  `json:"remaining"`
	Unit             string   `json:"unit"`
	// ResetAt 是 epoch 秒 —— tray 用 Date(timeIntervalSince1970:) 建
	// Date，发毫秒会渲染成 1970 年（与 Node 版 end/1000 对齐）。
	ResetAt *float64 `json:"resetAt,omitempty"`
}

// AccountSnapshot 是一个 provider 的账号用量快照。
type AccountSnapshot struct {
	Status       string   `json:"status"` // available | not-configured | error
	Source       string   `json:"source"`
	Provider     string   `json:"provider"`
	Metrics      []Metric `json:"metrics"`
	Plan         string   `json:"plan,omitempty"`
	DashboardURL string   `json:"dashboardUrl,omitempty"`
	Error        string   `json:"error,omitempty"`
}

const accountRequestTimeout = 10 * time.Second

// ---- opencode Go 订阅窗口 ----

// OpencodeGoUsageMetrics：usage.{rolling,weekly,monthly}.percent →
// 窗口指标（rolling 窗口时长不在载荷里，标签保持泛化）。
func OpencodeGoUsageMetrics(payload map[string]any) []Metric {
	usage, _ := payload["usage"].(map[string]any)
	if usage == nil {
		return nil
	}
	window := func(label string, detail any) *Metric {
		m, ok := detail.(map[string]any)
		if !ok {
			return nil
		}
		percent, ok := m["percent"].(float64)
		if !ok {
			return nil
		}
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		metric := Metric{
			Kind: "quota", Label: label,
			UsedPercent: percent, RemainingPercent: 100 - percent,
			Used: percent, Limit: 100, Remaining: 100 - percent, Unit: "percent",
		}
		for _, key := range []string{"resetsAt", "reset_at"} {
			if v, ok := m[key].(float64); ok {
				reset := v
				metric.ResetAt = &reset
				break
			}
		}
		return &metric
	}
	var metrics []Metric
	for _, entry := range []struct {
		label string
		key   string
	}{
		{"Rolling limit", "rolling"},
		{"Weekly limit", "weekly"},
		{"Monthly limit", "monthly"},
	} {
		if metric := window(entry.label, usage[entry.key]); metric != nil {
			metrics = append(metrics, *metric)
		}
	}
	return metrics
}

// ---- zai-coding 套餐配额 ----

// zaiQuotaURL 是套餐配额端点（测试可覆盖）。
var zaiQuotaURL = "https://api.z.ai/api/monitor/usage/quota/limit"

func setZAIQuotaURL(url string) { zaiQuotaURL = url }

const ZAIDashboardURL = "https://z.ai/manage-apikey/coding-plan/personal/my-plan"

func zaiWindowLabel(unit, number float64) string {
	switch int(unit) {
	case 6:
		if number == 1 {
			return "Weekly limit"
		}
		return fmt.Sprintf("%v-week limit", number)
	case 1:
		if number == 1 {
			return "Daily limit"
		}
		return fmt.Sprintf("%v-day limit", number)
	case 3:
		if number == 1 {
			return "Hourly limit"
		}
		return fmt.Sprintf("%v-hour limit", number)
	case 5:
		return fmt.Sprintf("%v-minute limit", number)
	}
	return ""
}

// ZAIQuotaMetrics：limits[] 优先计数器（usage=allowance、currentValue=已用；
// percentage 字段偶有陈旧/为零），TOKENS_LIMIT 标签带 tokens 后缀；
// unit=5 & number=1 的 TIME_LIMIT 是 MCP 月度标记不是可用窗口。
func ZAIQuotaMetrics(data map[string]any) []Metric {
	limits, _ := data["limits"].([]any)
	var metrics []Metric
	for _, raw := range limits {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		entryType, _ := entry["type"].(string)
		unit, _ := entry["unit"].(float64)
		number, _ := entry["number"].(float64)
		if entryType == "TIME_LIMIT" && int(unit) == 5 && number == 1 {
			continue
		}
		allowance, hasAllowance := entry["usage"].(float64)
		consumed, hasConsumed := entry["currentValue"].(float64)
		percent := -1.0
		if hasAllowance && hasConsumed && allowance > 0 {
			percent = consumed / allowance * 100
		} else if v, ok := entry["percentage"].(float64); ok {
			percent = v
		}
		if percent < 0 {
			continue
		}
		if percent > 100 {
			percent = 100
		}
		window := zaiWindowLabel(unit, number)
		label := window
		if entryType == "TOKENS_LIMIT" {
			if window == "" {
				label = "Token quota"
			} else {
				label = strings.Replace(window, " limit", " tokens", 1)
			}
		}
		if label == "" {
			continue
		}
		metric := Metric{
			Kind: "quota", Label: label,
			UsedPercent: percent, RemainingPercent: 100 - percent,
			Used: percent, Limit: 100, Remaining: 100 - percent, Unit: "percent",
		}
		if reset, ok := entry["nextResetTime"].(float64); ok && reset > 0 {
			resetMs := reset / 1000
			metric.ResetAt = &resetMs
		}
		metrics = append(metrics, metric)
	}
	return metrics
}

// ---- 账号拉取 ----

// FetchAccount 用给定凭据拉取 provider 账号用量。
func FetchAccount(ctx context.Context, client *http.Client, providerID, credential string) AccountSnapshot {
	if client == nil {
		client = &http.Client{Timeout: accountRequestTimeout}
	}
	snapshot := AccountSnapshot{Provider: providerID, Source: "official-api"}
	if credential == "" {
		snapshot.Status = "not-configured"
		return snapshot
	}
	switch providerID {
	case "zai-coding":
		return fetchZAI(ctx, client, credential)
	case "opencode-go":
		return fetchOpencode(ctx, client, credential)
	default:
		snapshot.Status = "error"
		snapshot.Error = "no account endpoint for " + providerID
		return snapshot
	}
}

func fetchZAI(ctx context.Context, client *http.Client, credential string) AccountSnapshot {
	snapshot := AccountSnapshot{Provider: "zai-coding", Source: "official-api", DashboardURL: ZAIDashboardURL}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zaiQuotaURL, nil)
	if err != nil {
		snapshot.Status, snapshot.Error = "error", err.Error()
		return snapshot
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := client.Do(req)
	if err != nil {
		snapshot.Status, snapshot.Error = "error", err.Error()
		return snapshot
	}
	defer resp.Body.Close()
	var payload struct {
		Success bool            `json:"success"`
		Code    int             `json:"code"`
		Msg     string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || !payload.Success || payload.Code != 200 {
		detail := payload.Msg
		if detail == "" {
			detail = fmt.Sprintf("Z.ai quota API returned HTTP %d", resp.StatusCode)
		}
		snapshot.Status, snapshot.Error = "error", detail
		return snapshot
	}
	var data map[string]any
	json.Unmarshal(payload.Data, &data)
	snapshot.Metrics = ZAIQuotaMetrics(data)
	if snapshot.Metrics == nil {
		snapshot.Status, snapshot.Error = "error", "Z.ai quota response was incomplete"
		return snapshot
	}
	if plan, ok := data["planName"].(string); ok && plan != "" {
		snapshot.Plan = plan
	}
	snapshot.Status = "available"
	return snapshot
}

func fetchOpencode(ctx context.Context, client *http.Client, credential string) AccountSnapshot {
	snapshot := AccountSnapshot{Provider: "opencode-go", Source: "official-api"}
	// 订阅用量只在官方端点上提供；自定义端点无法保证该路由存在。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://opencode.ai/zen/go/v1/usage", nil)
	if err != nil {
		snapshot.Status, snapshot.Error = "error", err.Error()
		return snapshot
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := client.Do(req)
	if err != nil {
		snapshot.Status, snapshot.Error = "error", err.Error()
		return snapshot
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		snapshot.Status, snapshot.Error = "error", err.Error()
		return snapshot
	}
	snapshot.Metrics = OpencodeGoUsageMetrics(payload)
	if snapshot.Metrics == nil {
		snapshot.Status, snapshot.Error = "error", "opencode usage response was incomplete"
		return snapshot
	}
	snapshot.Status = "available"
	return snapshot
}

// ---- ChatGPT 账号（Codex app-server JSON-RPC）----

// CodexAccountUsage 是原生订阅的用量快照。
type CodexAccountUsage struct {
	FetchedAt string             `json:"fetchedAt"`
	PlanType  string             `json:"planType,omitempty"`
	LimitID   string             `json:"limitId,omitempty"`
	Primary   *CodexWindow       `json:"primary,omitempty"`
	Secondary *CodexWindow       `json:"secondary,omitempty"`
	Buckets   []CodexDayBucket   `json:"dailyUsageBuckets"`
	// Summary 键本身非可选（tray 的 CodexUsageSummary）：哪怕字段全
	// 空也要发一个对象，缺键会让整个快照解码失败。
	Summary CodexUsageSummaryJSON `json:"summary"`
}

// CodexUsageSummaryJSON 的字段以 null 表达缺失（Node 版语义），tray
// 侧全部按可选解码。
type CodexUsageSummaryJSON struct {
	LifetimeTokens   *int64 `json:"lifetimeTokens"`
	PeakDailyTokens  *int64 `json:"peakDailyTokens"`
	CurrentStreakDays *int  `json:"currentStreakDays"`
}

// CodexWindow 是一个 5h/周窗口。
type CodexWindow struct {
	UsedPercent        float64  `json:"usedPercent"`
	RemainingPercent   float64  `json:"remainingPercent"`
	WindowDurationMins *float64 `json:"windowDurationMins"`
	ResetsAt           *float64 `json:"resetsAt"`
}

// CodexDayBucket 是一天的 token 消耗。
type CodexDayBucket struct {
	StartDate string  `json:"startDate"`
	Tokens    float64 `json:"tokens"`
}

// ReadCodexAccountUsage 经 `codex app-server` 的 JSON-RPC 读取账号窗口：
// initialize → rateLimits/read + usage/read → 汇总。无凭据/无 codex 时
// 返回错误（tray 显示 unavailable 而不是假数字）。
func ReadCodexAccountUsage(ctx context.Context, codexBinary string) (*CodexAccountUsage, error) {
	if codexBinary == "" {
		return nil, fmt.Errorf("no Codex binary found")
	}
	cmdCtx, cancel := context.WithTimeout(ctx, accountRequestTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, codexBinary, "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("the Codex app-server could not be started")
	}
	defer func() {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	send := func(v any) {
		raw, _ := json.Marshal(v)
		stdin.Write(append(raw, '\n'))
	}
	// 初始化握手 + 两个读取。
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo":   map[string]any{"name": "codex_router_go", "title": "Model Router", "version": "0.1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}})

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	responses := map[int]json.RawMessage{}
	for scanner.Scan() {
		var message struct {
			ID     *int            `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.ID == nil {
			continue
		}
		switch *message.ID {
		case 1:
			if message.Error != nil {
				return nil, fmt.Errorf("Codex app-server initialization failed")
			}
			send(map[string]any{"method": "initialized", "params": map[string]any{}})
			send(map[string]any{"id": 2, "method": "account/rateLimits/read", "params": nil})
			send(map[string]any{"id": 3, "method": "account/usage/read", "params": nil})
		case 2:
			if message.Error != nil {
				return nil, fmt.Errorf("Codex account limits are unavailable for this login")
			}
			responses[2] = message.Result
		case 3:
			responses[3] = message.Result
			if message.Error != nil {
				responses[3] = json.RawMessage(`{"summary":{},"dailyUsageBuckets":[]}`)
			}
		}
		if len(responses) == 2 {
			return normalizeCodexUsage(responses[2], responses[3]), nil
		}
	}
	if cmdCtx.Err() != nil {
		return nil, fmt.Errorf("Codex account usage request timed out")
	}
	return nil, fmt.Errorf("Codex app-server exited before replying")
}

func normalizeCodexUsage(rateLimitsRaw, usageRaw json.RawMessage) *CodexAccountUsage {
	usage := CodexAccountUsage{FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	var rateLimits struct {
		RateLimits struct {
			PlanType  string          `json:"planType"`
			LimitID   string          `json:"limitId"`
			Primary   codexWindowJSON `json:"primary"`
			Secondary codexWindowJSON `json:"secondary"`
		} `json:"rateLimits"`
	}
	json.Unmarshal(rateLimitsRaw, &rateLimits)
	var usagePayload struct {
		Summary struct {
			LifetimeTokens    *float64 `json:"lifetimeTokens"`
			PeakDailyTokens   *float64 `json:"peakDailyTokens"`
			CurrentStreakDays *float64 `json:"currentStreakDays"`
		} `json:"summary"`
		DailyUsageBuckets []struct {
			StartDate string  `json:"startDate"`
			Tokens    float64 `json:"tokens"`
		} `json:"dailyUsageBuckets"`
	}
	json.Unmarshal(usageRaw, &usagePayload)

	usage.PlanType = rateLimits.RateLimits.PlanType
	usage.LimitID = rateLimits.RateLimits.LimitID
	if w := rateLimits.RateLimits.Primary.Normalize(); w != nil {
		usage.Primary = w
	}
	if w := rateLimits.RateLimits.Secondary.Normalize(); w != nil {
		usage.Secondary = w
	}
	for _, bucket := range usagePayload.DailyUsageBuckets {
		if len(bucket.StartDate) == 10 && bucket.Tokens >= 0 {
			usage.Buckets = append(usage.Buckets, CodexDayBucket{
				StartDate: bucket.StartDate, Tokens: bucket.Tokens,
			})
		}
	}
	// summary 的字段以 null 表达「上游没给」，数值非法同样归 null。
	if usagePayload.Summary.LifetimeTokens != nil && *usagePayload.Summary.LifetimeTokens >= 0 {
		v := int64(*usagePayload.Summary.LifetimeTokens)
		usage.Summary.LifetimeTokens = &v
	}
	if usagePayload.Summary.PeakDailyTokens != nil && *usagePayload.Summary.PeakDailyTokens >= 0 {
		v := int64(*usagePayload.Summary.PeakDailyTokens)
		usage.Summary.PeakDailyTokens = &v
	}
	if usagePayload.Summary.CurrentStreakDays != nil && *usagePayload.Summary.CurrentStreakDays >= 0 {
		v := int(*usagePayload.Summary.CurrentStreakDays)
		usage.Summary.CurrentStreakDays = &v
	}
	return &usage
}

type codexWindowJSON struct {
	UsedPercent        float64  `json:"usedPercent"`
	WindowDurationMins *float64 `json:"windowDurationMins"`
	ResetsAt           *float64 `json:"resetsAt"`
}

func (w codexWindowJSON) Normalize() *CodexWindow {
	percent := w.UsedPercent
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return &CodexWindow{
		UsedPercent: percent, RemainingPercent: 100 - percent,
		WindowDurationMins: w.WindowDurationMins, ResetsAt: w.ResetsAt,
	}
}
