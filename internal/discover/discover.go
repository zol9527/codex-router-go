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

// familyCloneSource 选克隆源：同 provider 家族里 upstream 与目标 ID
// 公共前缀最长的模型（glm-5-4 → glm-5.3 而不是 glm-4.7）；provider
// 无模型时返回 nil（字段走零值，靠 models.dev 补齐）。
func familyCloneSource(reg *registry.Registry, providerID, targetID string) *registry.Model {
	target := NormalizeID(targetID)
	var best *registry.Model
	bestLen := -1
	for _, m := range reg.Models {
		if reg.CanonicalProviderID(m.Provider) != reg.CanonicalProviderID(providerID) {
			continue
		}
		n := commonPrefixLen(NormalizeID(m.UpstreamModel), target)
		if best == nil || n > bestLen {
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
	}
	if clone != nil {
		model.DisplayName = clone.DisplayName
		model.Description = clone.Description
		model.Priority = clone.Priority
		model.DefaultEffort = clone.DefaultEffort
		model.ReasoningLevels = clone.ReasoningLevels
		model.ContextWindow = clone.ContextWindow
		model.AutoCompact = clone.AutoCompact
		model.InputModalities = clone.InputModalities
		model.RequestProfile = clone.RequestProfile
		model.CompHash = clone.CompHash
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
