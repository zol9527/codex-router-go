// Package catalog 生成 Codex picker 用的 merged-models.json：
// `codex debug models` 抓取的原生目录 + 注册表路由条目合并。
// 简化自 catalog.mjs（砍掉公告、multi-target、版本 clamp）。
package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// NativeModel 是 Codex 原生目录里的一个条目（只声明本 fork 读取的字段，
// 其余字段透传保留 —— 原生条目原样进合并目录）。
type NativeModel map[string]any

type nativeCatalog struct {
	Models []NativeModel `json:"models"`
}

// RoutedModel 把注册表条目转成 Codex picker 形状。
// 字段集合以 catalog.mjs 的 routedModel() 为准（简化版砍掉
// availability_nux / upgrade / service_tiers / search 能力声明）。
func RoutedModel(template NativeModel, m *registry.Model) NativeModel {
	next := NativeModel{}
	for k, v := range template {
		next[k] = v
	}
	levels := make([]any, 0, len(m.ReasoningLevels))
	for _, level := range m.ReasoningLevels {
		entry := map[string]any{"effort": level.Effort}
		if level.Description != "" {
			entry["description"] = level.Description
		}
		levels = append(levels, entry)
	}

	next["slug"] = m.Slug
	next["display_name"] = m.DisplayName
	next["description"] = m.Description
	next["priority"] = m.Priority
	next["visibility"] = "list"
	next["supported_in_api"] = true
	next["default_reasoning_level"] = m.DefaultEffort
	next["supported_reasoning_levels"] = levels
	next["context_window"] = m.ContextWindow
	next["max_context_window"] = m.ContextWindow
	next["effective_context_window_percent"] = 95
	if m.AutoCompact > 0 {
		next["auto_compact_token_limit"] = m.AutoCompact
	} else {
		next["auto_compact_token_limit"] = m.ContextWindow * 9 / 10
	}
	// input_modalities 是序列（Node 版传数组；原生条目的字符串形态是
	// Codex 自己的形状，透传无妨，但 routed 条目必须按规格给数组）。
	modalities := m.InputModalities
	if len(modalities) == 0 {
		modalities = []string{"text"}
	}
	next["input_modalities"] = modalities
	// comp_hash 刻意删除：克隆源模板（native 条目）自带 comp_hash（如
	// luna 的 "3000"），而 Codex 只在两侧都有值且不同时把模型切换判定为
	// 需要交接压缩（CompHashChanged）。子代理 fork 会继承父线程的
	// previous_turn_settings（native 父模型），与外部模型的 hash 必不同
	// —— 每次派发都开场白压一次（重放全量上下文，约 20s）。routed 条目
	// 不声明 compaction 兼容组，任一侧缺失即跳过该压缩（Codex 测试
	// pre_sampling_compact_skips_when_either_comp_hash_is_missing 覆盖的
	// 语义）；token 超限与降窗压缩不读该字段，不受影响。
	delete(next, "comp_hash")
	next["additional_speed_tiers"] = []any{}
	next["default_service_tier"] = nil
	// supports_parallel_tool_calls：桌面端 2026-08-19 起把该字段列为
	// 必填（serde 无默认），缺失会让整个 model_catalog_json 解析失败、
	// picker 退回纯原生目录（2026-08-20 实发 "missing field" 全量拒收）。
	// chat 翻译层对多条 tool_calls 数组透明，按原生行为声明支持。
	next["supports_parallel_tool_calls"] = true
	next["supports_reasoning_summaries"] = false
	next["default_reasoning_summary"] = "none"
	next["support_verbosity"] = false
	next["default_verbosity"] = nil
	next["use_responses_lite"] = false
	next["apply_patch_tool_type"] = "freeform"
	// Codex v2 协作只把 catalog 里同标 v2 的模型暴露为 spawn 候选；
	// 未证明的模型保持 v1（不出现在分身选择里）。
	multiAgent := m.MultiAgentVersion
	if multiAgent == "" {
		multiAgent = "v1"
	}
	next["multi_agent_version"] = multiAgent
	return next
}

// FetchNative 抓取 Codex 原生目录：账号态优先，bundled 补充。
// 两者都不带 slug 重复；账号态条目优先。
func FetchNative(codexBinary string) ([]NativeModel, error) {
	account, errAccount := runDebugModels(codexBinary, false)
	bundled, errBundled := runDebugModels(codexBinary, true)
	if errAccount != nil && errBundled != nil {
		return nil, fmt.Errorf("codex debug models failed: %v / %v", errAccount, errBundled)
	}
	seen := map[string]bool{}
	merged := []NativeModel{}
	for _, source := range [][]NativeModel{account, bundled} {
		for _, model := range source {
			slug, _ := model["slug"].(string)
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true
			// 桌面端 2026-08-19 起必填 supports_parallel_tool_calls（见
			// RoutedModel 同名注释）。codex CLI（0.148）的 debug models 输出
			// 还没带它 —— 缺失时补 true（GPT 系原生行为），CLI 补齐后透传。
			if _, ok := model["supports_parallel_tool_calls"]; !ok {
				model["supports_parallel_tool_calls"] = true
			}
			merged = append(merged, model)
		}
	}
	return merged, nil
}

func runDebugModels(codexBinary string, bundled bool) ([]NativeModel, error) {
	args := []string{"debug", "models"}
	if bundled {
		args = append(args, "--bundled")
	}
	cmd := exec.Command(codexBinary, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var parsed nativeCatalog
	if err := json.Unmarshal(bytes.TrimSpace(out), &parsed); err != nil {
		return nil, fmt.Errorf("parse codex debug models output: %w", err)
	}
	return parsed.Models, nil
}

// Build 合并原生目录与注册表条目。includeNative=false 用于
// "只发布路由模型" 的场景（本 fork 默认包含原生条目，Codex 需要
// 它们渲染原生 GPT 选择）。
func Build(native []NativeModel, regModels []*registry.Model, enabled func(providerID string) bool, includeNative bool, hidden map[string]bool) map[string]any {
	models := []NativeModel{}
	// 反馈环切断：路由器把自己的 catalog 发布进 Codex（model_catalog_json），
	// `codex debug models` 随后会把路由模型当成"原生"条目抓回来。不过滤
	// 的话，同一个模型在本目录里出现两份（反馈份 + 注册表份），picker
	// 随之翻倍 —— 连已从注册表删除的模型都会从 Codex 的记忆里还魂。
	// 规则：带 "/" 的 slug 一律是路由条目；再按 gateway_model 双保险。
	//
	// 注意 upstream_model 绝不能进反馈键：路由器所有发布面（merged 目录、
	// /v1/models）都只输出完整路由 slug，反馈环不可能产生裸 upstream 名；
	// 反倒是一场实发事故（2026-08-15 gpt-5.6-luna）——models.dev 把原生
	// 模型挂到网关下生成克隆（opencode-go-responses/gpt-5.6-luna，其
	// upstream_model 恰是原生 slug），upstream 进反馈键会把真原生条目
	// 一起误杀，picker 里 luna 凭空消失。
	routedKeys := map[string]bool{}
	for _, m := range regModels {
		routedKeys[m.Slug] = true
		if m.GatewayModel != "" {
			routedKeys[m.GatewayModel] = true
		}
	}
	filteredNative := make([]NativeModel, 0, len(native))
	for _, entry := range native {
		slug, _ := entry["slug"].(string)
		gateway, _ := entry["gateway_model"].(string)
		if hidden[slug] || strings.Contains(slug, "/") || routedKeys[slug] || routedKeys[gateway] {
			continue
		}
		filteredNative = append(filteredNative, entry)
	}
	if includeNative {
		models = append(models, filteredNative...)
	}
	// 模板：取账号目录第一个真原生条目（字段形状参考）；没有时用最小模板。
	template := NativeModel{}
	if len(filteredNative) > 0 {
		for k, v := range filteredNative[0] {
			template[k] = v
		}
	}
	for _, model := range regModels {
		if !model.Listed {
			continue
		}
		if !enabled(model.Provider) {
			continue
		}
		// 隐藏 = 不进 picker 目录；按名路由不受影响。
		if hidden[model.Slug] {
			continue
		}
		models = append(models, RoutedModel(template, model))
	}
	return map[string]any{"models": models}
}

// Write 落盘 merged-models.json（0600）。
func Write(path string, catalog map[string]any) error {
	raw, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Refresh 一把梭：抓取 + 子代理设置应用 + 合并 + 写盘 + agents 目录
// 同步。返回模型数。
func Refresh(codexBinary, outputPath string, reg *registry.Registry, enabled func(providerID string) bool) (int, error) {
	native, err := FetchNative(codexBinary)
	if err != nil {
		return 0, err
	}
	// 子代理设置住在 state 目录（与 merged-models.json 同目录）。
	settings := state.ReadSubagentSettings(filepath.Dir(outputPath))
	hidden := state.ReadPickerHidden(filepath.Dir(outputPath))
	native = PromoteNativeMultiAgent(native, settings)
	routedModels := ApplySubagentMultiAgent(reg.Models, settings, hidden)
	catalog := Build(native, routedModels, enabled, true, hidden)
	if err := Write(outputPath, catalog); err != nil {
		return 0, err
	}
	// agents 目录同步：合格模型得到按名可 spawn 的定义；不再合格的
	// 定义必须删掉。合格集合只从"已启用 provider 的已列模型"里挑 ——
	// 给未启用 provider 的模型留定义，spawn 出来只会是一个注定失败的
	// 选项。失败不吞 —— 半同步比失败糟（但 Sync 自己会回滚）。
	enabledModels := []*registry.Model{}
	for _, m := range routedModels {
		if m.Listed && enabled(m.Provider) {
			enabledModels = append(enabledModels, m)
		}
	}
	if _, err := SyncRoutedCodexAgents(SubagentEligibleModels(enabledModels, settings), codexAgentsDir()); err != nil {
		return 0, fmt.Errorf("sync codex agents: %w", err)
	}
	models, _ := catalog["models"].([]NativeModel)
	return len(models), nil
}

// codexAgentsDir 是 Codex 的 agents 目录（CODEX_HOME/agents）。
func codexAgentsDir() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".codex", "agents")
		}
		home = filepath.Join(userHome, ".codex")
	}
	return filepath.Join(home, "agents")
}
