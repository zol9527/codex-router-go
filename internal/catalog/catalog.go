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
	next["comp_hash"] = m.CompHash
	next["additional_speed_tiers"] = []any{}
	next["default_service_tier"] = nil
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
func Build(native []NativeModel, regModels []*registry.Model, enabled func(providerID string) bool, includeNative bool) map[string]any {
	models := []NativeModel{}
	if includeNative {
		models = append(models, native...)
	}
	// 模板：取账号目录第一个条目（字段形状参考）；没有原生目录时
	// 用最小模板。
	template := NativeModel{}
	if len(native) > 0 {
		for k, v := range native[0] {
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
	native = PromoteNativeMultiAgent(native, settings)
	routedModels := ApplySubagentDemotions(reg.Models, settings)
	catalog := Build(native, routedModels, enabled, true)
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
