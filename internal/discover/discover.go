// Package discover：实时模型发现与动态注册。
//
// 两个数据源各司其职：
//   - provider 自己的 /v1/models —— 货架真值（有什么模型、确切 ID）
//   - models.dev 开源库 —— 参数真值（上下文窗口、推理、视觉、描述）
//
// 注册优先级：models.dev 命中 → 精确参数；未命中 → 同家族克隆兜底
// （effort 档位这类 models.dev 不表达的形状始终来自家族）。
// 写入 user-models.json 覆盖层；内嵌注册表已收录的模型不重复注册。
package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/modelmeta"
	"github.com/loyd/codex-router/internal/registry"
)

// FetchModels 拉取 OpenAI 兼容的 /v1/models 全量列表（大小写/别名去重）。
func FetchModels(ctx context.Context, client *http.Client, baseURL, credential string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("credential rejected (HTTP %d) — check the api_key", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, string(body))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	seen := map[string]bool{}
	for _, m := range payload.Data {
		key := NormalizeID(m.ID)
		if m.ID == "" || seen[key] {
			continue
		}
		seen[key] = true
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// NormalizeID：上游常见 alias 后缀（:free）与大小写差异不应制造假差异。
func NormalizeID(id string) string {
	return strings.ToLower(strings.SplitN(id, ":", 2)[0])
}

// SyncReport 记录一次同步的结果。
type SyncReport struct {
	Provider string
	Added    []registry.UserModelEntry
	Skipped  []string // 上游有、但（内嵌或覆盖层）已收录
	MetaHits int      // 参数来自 models.dev 的新模型数
	Clones   int      // 家族克隆兜底的新模型数
	Err      error
}

// Sync 对一个 provider 执行发现+注册：拉货架 → 对照（内嵌+覆盖层）→
// 新模型建模写入覆盖层。已收录的不动。
func Sync(ctx context.Context, stateDir string, reg *registry.Registry, providerID, credential string) SyncReport {
	report := SyncReport{Provider: providerID}
	p := reg.Providers[providerID]
	if p == nil {
		report.Err = fmt.Errorf("unknown provider %q", providerID)
		return report
	}
	baseURL := p.BaseURL
	if p.BaseURLEnv != "" {
		if v := os.Getenv(p.BaseURLEnv); v != "" {
			baseURL = v
		}
	}
	ids, err := FetchModels(ctx, &http.Client{Timeout: 20 * time.Second}, baseURL, credential)
	if err != nil {
		report.Err = err
		return report
	}

	// 已收录集合（内嵌 + 现有覆盖层），按 upstream/gateway/slug 三键。
	known := map[string]bool{}
	for _, m := range reg.Models {
		if reg.CanonicalProviderID(m.Provider) != reg.CanonicalProviderID(providerID) {
			continue
		}
		known[NormalizeID(m.UpstreamModel)] = true
		known[NormalizeID(m.GatewayModel)] = true
		known[NormalizeID(m.Slug)] = true
	}
	entries := registry.ReadUserModels(stateDir)
	overlaySlugs := map[string]bool{}
	for _, e := range entries {
		overlaySlugs[e.Model.Slug] = true
		known[NormalizeID(e.Model.UpstreamModel)] = true
	}

	for _, id := range ids {
		if known[NormalizeID(id)] {
			report.Skipped = append(report.Skipped, id)
			continue
		}
		entry := buildEntry(p, id, familyCloneSource(reg, providerID, id), stateDir, ctx)
		if entry.Model.Slug == "" || overlaySlugs[entry.Model.Slug] {
			continue
		}
		entries = append(entries, entry)
		overlaySlugs[entry.Model.Slug] = true
		if entry.Source == "modelsdev" {
			report.MetaHits++
		} else {
			report.Clones++
		}
		report.Added = append(report.Added, entry)
	}
	if len(report.Added) > 0 {
		if err := registry.WriteUserModels(stateDir, entries); err != nil {
			report.Err = fmt.Errorf("write overlay: %w", err)
			report.Added = nil
		}
	}
	return report
}

// familyCloneSource 选克隆源。models.dev 未收录的模型只能抄家族参数，
// 抄谁直接决定 ctx/档位这些猜测的保守程度：
//
//   - 前缀最近者优先（glm-5.1 → glm-5.2 而不是 glm-4.7：同代数形状最像）
//   - 前缀并列时取 ctx 更小者 —— 克隆是猜测，猜测宁可低估（宁可让
//     Codex 早压缩）也不虚标（虚标 1M 的 200K 模型会在长会话里溢出）
func familyCloneSource(reg *registry.Registry, providerID, targetID string) *registry.Model {
	target := NormalizeID(targetID)
	var best *registry.Model
	bestLen := -1
	for _, m := range reg.Models {
		if reg.CanonicalProviderID(m.Provider) != reg.CanonicalProviderID(providerID) {
			continue
		}
		n := commonPrefixLen(NormalizeID(m.UpstreamModel), target)
		if best == nil || n > bestLen ||
			(n == bestLen && m.ContextWindow < best.ContextWindow) {
			best, bestLen = m, n
		}
	}
	return best
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// BuildEntry 用 models.dev 元数据（命中）或家族克隆（兜底）为一个
// 上游模型构建覆盖层条目。克隆先铺底（effort 档位/画像/compHash 这类
// 家族形状），models.dev 命中后覆盖可确定的字段。
func BuildEntry(reg *registry.Registry, p *registry.Provider, upstreamID, stateDir string, ctx context.Context) registry.UserModelEntry {
	clone := familyCloneSource(reg, p.ID, upstreamID)
	return buildEntry(p, upstreamID, clone, stateDir, ctx)
}

// acronyms 是派生显示名时按全大写处理的模型家族缩写。
var acronyms = map[string]string{
	"glm":  "GLM",
	"gpt":  "GPT",
	"grok": "GROK",
	"qwen": "Qwen",
}

// prettifyModelID 从上游模型 ID 派生人类可读且唯一的显示名。
// 上游已带大写（如 "GLM-4.5-Air"）则原样使用；全小写（如 "glm-4.5-air"）
// 则按连字符分词：缩写全大写，其余词首字母大写 → "GLM-4.5-Air"。
func prettifyModelID(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return s
	}
	if strings.ToLower(s) != s {
		return s
	}
	parts := strings.Split(s, "-")
	for i, p := range parts {
		if up, ok := acronyms[p]; ok {
			parts[i] = up
			continue
		}
		if p != "" && p[0] >= 'a' && p[0] <= 'z' {
			parts[i] = string(p[0]-'a'+'A') + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

// planSuffix 提取显示名末尾的括号套餐后缀（" (Coding Plan)" 等），
// 用于克隆条目继承品牌后缀但不是克隆源的名字本身。
func planSuffix(display string) string {
	if i := strings.LastIndex(display, " ("); i >= 0 && strings.HasSuffix(display, ")") {
		return display[i:]
	}
	return ""
}

func buildEntry(p *registry.Provider, upstreamID string, clone *registry.Model, stateDir string, ctx context.Context) registry.UserModelEntry {
	now := time.Now().UTC().Format(time.RFC3339)
	family := p.ID
	if p.VariantOf != "" {
		family = p.VariantOf
	}
	normalized := NormalizeID(upstreamID)
	// slug 保留点号与现有命名一致（glm-5.4）；gateway 用连字符。
	slugTail := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			return r
		default:
			return '-'
		}
	}, normalized)
	slugTail = strings.Trim(slugTail, "-")
	gatewayTail := strings.ReplaceAll(slugTail, ".", "-")

	model := registry.Model{
		Slug:          family + "/" + slugTail,
		GatewayModel:  family + "-" + gatewayTail,
		UpstreamModel: upstreamID,
		Provider:      family,
		Listed:        true,
		DisplayName:   prettifyModelID(upstreamID),
	}
	if clone != nil {
		// 显示名绝不能继承克隆源的全名：否则 glm-4.5/glm-4.6 等未收录
		// models.dev 的模型会全部显示成 "GLM-5-Turbo (Coding Plan)"，
		// 在 Codex 选择表里看起来像重复项。显示名由上游 ID 派生（天然
		// 唯一），仅从克隆源继承括号里的套餐后缀（如 " (Coding Plan)"）。
		model.DisplayName = prettifyModelID(upstreamID) + planSuffix(clone.DisplayName)
		model.Description = clone.Description
		model.Priority = clone.Priority
		model.DefaultEffort = clone.DefaultEffort
		model.ReasoningLevels = clone.ReasoningLevels
		model.ContextWindow = clone.ContextWindow
		model.AutoCompact = clone.AutoCompact
		model.InputModalities = clone.InputModalities
		model.RequestProfile = clone.RequestProfile
		model.CompHash = clone.CompHash
	} else if p.DefaultContextWindow > 0 {
		model.ContextWindow = p.DefaultContextWindow
		model.AutoCompact = p.DefaultContextWindow * 9 / 10
	}
	entry := registry.UserModelEntry{Model: model, Source: "clone", AddedAt: now}

	if p.ModelsDevID != "" {
		if meta, ok, _ := modelmeta.Lookup(ctx, stateDir, p.ModelsDevID, upstreamID); ok {
			if meta.Name != "" {
				model.DisplayName = meta.Name
			}
			if meta.Description != "" {
				model.Description = meta.Description
			}
			if meta.Context > 0 {
				model.ContextWindow = meta.Context
				model.AutoCompact = meta.Context * 9 / 10
			}
			if len(meta.InputModes) > 0 {
				model.InputModalities = meta.InputModes
			}
			entry.Source = "modelsdev"
		}
	}
	entry.Model = model
	return entry
}
