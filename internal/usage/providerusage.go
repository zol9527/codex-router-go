package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 本文件是 tray ProviderUsageSnapshot 解码器（ModelRouterTrayApp.swift）
// 的对端：把 usage-events.jsonl 聚合成按 provider 合并的用量视图。
// 语义逐条移植自 Node 版 src/provider-usage.mjs —— 字段名、中位数算法、
// 不可信速率剔除都与 tray 的显示逻辑耦合，改动前先看那边的注释。

// maxPlausibleTokensPerSecond：已发布的最快出词率只有几百 tok/s，
// 阈值远高于它，使得真正快的模型永不被误杀，而检测失败的样本
//（首 token 在流末尾才被察觉，几千 token 看似毫秒到达）被剔除。
const maxPlausibleTokensPerSecond = 500

// speedSampleWindow：显示速率取最近 N 条的中位数 —— 平滑单发尖峰，
// 又不让 90 天前的旧会话主导当前读数。
const speedSampleWindow = 20

type ProviderSeed struct {
	ID             string
	DisplayName    string
	CredentialType string // "api" | "oauth" | "anonymous"
}

type ProviderUsageSnapshot struct {
	FetchedAt string               `json:"fetchedAt"`
	Scope     string               `json:"scope"`
	Providers []TrayProviderUsage  `json:"providers"`
}

type TrayProviderUsage struct {
	ID                 string            `json:"id"`
	DisplayName        string            `json:"displayName"`
	CredentialType     string            `json:"credentialType"`
	Scope              string            `json:"scope"`
	Requests           int               `json:"requests"`
	SuccessfulRequests int               `json:"successfulRequests"`
	MeteredRequests    int               `json:"meteredRequests"`
	InputTokens        int64             `json:"inputTokens"`
	OutputTokens       int64             `json:"outputTokens"`
	TotalTokens        int64             `json:"totalTokens"`
	DailyUsageBuckets  []TrayDailyBucket `json:"dailyUsageBuckets"`
	Models             []TrayModelUsage  `json:"models"`
	Account            TrayAccount       `json:"account"`
}

type TrayDailyBucket struct {
	StartDate string `json:"startDate"`
	Tokens    int64  `json:"tokens"`
	Requests  int    `json:"requests"`
}

type TrayModelUsage struct {
	Slug string `json:"slug"`
	// 路由 slug 带 provider 前缀（kimi-oauth/k3）；tray 已按 provider
	// 分组，这里只显示尾巴。原生 slug（gpt-5.6-sol）无斜杠，原样。
	DisplayName             string   `json:"displayName"`
	Requests                int      `json:"requests"`
	SuccessfulRequests      int      `json:"successfulRequests"`
	MeteredRequests         int      `json:"meteredRequests"`
	InputTokens             int64    `json:"inputTokens"`
	OutputTokens            int64    `json:"outputTokens"`
	TotalTokens             int64    `json:"totalTokens"`
	SpeedSampleCount        int      `json:"speedSampleCount"`
	ObservedFirstTokenMs    *int64   `json:"observedFirstTokenMs"`
	ObservedTokensPerSecond *float64 `json:"observedTokensPerSecond"`
	LastUsedAt              string   `json:"lastUsedAt"`
}

// TrayAccount 是 provider 的账号配额块；tray 把它解码为非可选字段，
// 账号层未覆盖的 provider（原生 openai）也要给 local-only 兜底。
type TrayAccount struct {
	Status       string   `json:"status"`
	Source       string   `json:"source"`
	Metrics      []Metric `json:"metrics"`
	Message      *string  `json:"message,omitempty"`
	Plan         string   `json:"plan,omitempty"`
	DashboardURL string   `json:"dashboardUrl,omitempty"`
}

// LocalOnlyAccount：账号层不覆盖的 provider 的兜底 —— 只有本路由器
// 观察到的流量，订阅配额走另一条 Codex account 路径。
func LocalOnlyAccount() TrayAccount {
	message := "Router-observed traffic; subscription quota is tracked separately."
	return TrayAccount{
		Status: "local-only", Source: "local-router", Metrics: []Metric{}, Message: &message,
	}
}

type speedSample struct {
	outputTokens        int64
	generationDurationMs int64
}

type modelAcc struct {
	usage           TrayModelUsage
	speedSamples    []speedSample
	firstTokenMs    []int64
}

type providerAcc struct {
	row    TrayProviderUsage
	daily  map[string]*TrayDailyBucket
	models map[string]*modelAcc
	order  []string // 模型插入序，稳定输出
}

// BuildProviderUsageSnapshot 聚合近 90 天事件。account 块留给调用方
// 合并（那里才有凭据与 HTTP 客户端）。
func BuildProviderUsageSnapshot(stateDir string, seeds []ProviderSeed, now time.Time) ProviderUsageSnapshot {
	byProvider := map[string]*providerAcc{}
	ordered := []*providerAcc{}
	for _, seed := range seeds {
		acc := &providerAcc{
			row: TrayProviderUsage{
				ID: seed.ID, DisplayName: seed.DisplayName,
				CredentialType: seed.CredentialType, Scope: "local-router",
			},
			daily:  map[string]*TrayDailyBucket{},
			models: map[string]*modelAcc{},
		}
		byProvider[seed.ID] = acc
		ordered = append(ordered, acc)
	}

	cutoff := now.AddDate(0, 0, -90)
	// 无 token 字段的事件（视觉桥转写只记状态不记 token）不计入 ——
	// 与 Node 版按 meteringVersion 过滤的意图一致。
	countEvent := func(e *Event, at time.Time) bool {
		return e.InputTokens != 0 || e.OutputTokens != 0 || e.TotalTokens != 0
	}

	if file, err := os.Open(filepath.Join(stateDir, "usage-events.jsonl")); err == nil {
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) != nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, e.At)
			if err != nil || at.Before(cutoff) || at.After(now) {
				continue
			}
			acc := byProvider[e.Provider]
			if acc == nil || !countEvent(&e, at) {
				continue
			}
			applyEvent(acc, &e, at)
		}
		file.Close()
	}

	snapshot := ProviderUsageSnapshot{
		FetchedAt: now.UTC().Format(time.RFC3339), Scope: "local-router",
	}
	for _, acc := range ordered {
		finalizeProvider(acc)
		snapshot.Providers = append(snapshot.Providers, acc.row)
	}
	return snapshot
}

func applyEvent(acc *providerAcc, e *Event, at time.Time) {
	row := &acc.row
	row.Requests++
	success := e.Status >= 200 && e.Status < 400
	if success {
		row.SuccessfulRequests++
	}
	row.MeteredRequests++
	total := e.TotalTokens
	if total == 0 && (e.InputTokens != 0 || e.OutputTokens != 0) {
		total = e.InputTokens + e.OutputTokens
	}
	row.InputTokens += nonneg64(e.InputTokens)
	row.OutputTokens += nonneg64(e.OutputTokens)
	row.TotalTokens += nonneg64(total)

	day := at.Local().Format("2006-01-02")
	bucket := acc.daily[day]
	if bucket == nil {
		bucket = &TrayDailyBucket{StartDate: day}
		acc.daily[day] = bucket
	}
	bucket.Tokens += nonneg64(total)
	bucket.Requests++

	slug := e.Model
	if slug == "" {
		slug = "unknown"
	}
	model := acc.models[slug]
	if model == nil {
		model = &modelAcc{}
		model.usage.Slug = slug
		model.usage.DisplayName = modelDisplayName(slug)
		model.usage.LastUsedAt = at.UTC().Format(time.RFC3339)
		acc.models[slug] = model
		acc.order = append(acc.order, slug)
	}
	m := &model.usage
	m.Requests++
	if success {
		m.SuccessfulRequests++
	}
	m.MeteredRequests++
	m.InputTokens += nonneg64(e.InputTokens)
	m.OutputTokens += nonneg64(e.OutputTokens)
	m.TotalTokens += nonneg64(total)
	if iso := at.UTC().Format(time.RFC3339); iso >= m.LastUsedAt {
		m.LastUsedAt = iso
	}

	// 出词率定义为首 token 之后 的速率；等待时间另计为 TTFT。
	// 除以响应头以来的时间会把推理模型的静默思考埋进分母。
	duration := nonneg64(e.DurationMs)
	firstToken := e.FirstTokenMs
	generation := duration
	if firstToken > 0 && firstToken <= duration {
		generation = duration - firstToken
	}
	// 长 Codex 回合可能触发空补全守卫预算后仍以 200 正常完成 ——
	// 该标记只说明「停止等待分类空补全」，不代表速率不可用。
	// 空补全/重试/取消的回复才剔除。
	measurable := success && e.Retries == 0 && !e.EmptyCompletion && !e.EmptyCompletionRetried
	output := nonneg64(e.OutputTokens)
	if measurable && output > 0 && firstToken > 0 && generation > 0 {
		impossible := float64(output)*1000/float64(generation) > maxPlausibleTokensPerSecond
		if !impossible {
			model.speedSamples = append(model.speedSamples, speedSample{output, generation})
			if len(model.speedSamples) > speedSampleWindow {
				model.speedSamples = model.speedSamples[1:]
			}
		}
	}
	if measurable && firstToken > 0 && firstToken <= duration {
		model.firstTokenMs = append(model.firstTokenMs, firstToken)
		if len(model.firstTokenMs) > speedSampleWindow {
			model.firstTokenMs = model.firstTokenMs[1:]
		}
	}
}

func finalizeProvider(acc *providerAcc) {
	for _, day := range sortedKeys(acc.daily) {
		acc.row.DailyUsageBuckets = append(acc.row.DailyUsageBuckets, *acc.daily[day])
	}
	for _, slug := range acc.order {
		model := acc.models[slug]
		finalizeModel(model)
		acc.row.Models = append(acc.row.Models, model.usage)
	}
	// 按 token 总量降序，再按请求数 —— tray 的「按模型」表沿用这个次序。
	sort.SliceStable(acc.row.Models, func(i, j int) bool {
		if acc.row.Models[i].TotalTokens != acc.row.Models[j].TotalTokens {
			return acc.row.Models[i].TotalTokens > acc.row.Models[j].TotalTokens
		}
		return acc.row.Models[i].Requests > acc.row.Models[j].Requests
	})
}

func finalizeModel(model *modelAcc) {
	m := &model.usage
	m.SpeedSampleCount = len(model.speedSamples)
	// 单条速率的中位数，不是总量除以总时长 —— 合并比值会让一个坏样本
	// 决定答案；中位数动不了少数派，也无需调阈值。
	rates := make([]float64, 0, len(model.speedSamples))
	for _, s := range model.speedSamples {
		rates = append(rates, float64(s.outputTokens)*1000/float64(s.generationDurationMs))
	}
	if len(rates) > 0 {
		sort.Float64s(rates)
		median := rates[len(rates)/2]
		rounded := float64(int64(median*10+0.5)) / 10
		m.ObservedTokensPerSecond = &rounded
	}
	if len(model.firstTokenMs) > 0 {
		sorted := append([]int64(nil), model.firstTokenMs...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		median := sorted[len(sorted)/2]
		m.ObservedFirstTokenMs = &median
	}
}

func modelDisplayName(slug string) string {
	for i := len(slug) - 1; i >= 0; i-- {
		if slug[i] == '/' {
			if tail := slug[i+1:]; tail != "" {
				return tail
			}
			break
		}
	}
	return slug
}

func nonneg64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
