package translate

// codex-app-tools 合并，移植自 codex-app-tools.mjs。
//
// Codex app 把自己的工具集（threads / automations / navigation / 插件
// 管理）以 deferLoading 注册并在客户端原生执行，但发给路由请求的
// codex_app namespace 是精简版。合并完整快照让路由模型看到与原生
// 模型相同的工具；router 只转发定义与结果，从不自己执行 app 工具。
//
// 快照数据用 go:embed 携带（源：2026-08-09 从 live Codex Desktop 会话
// 抓取的 dynamic_tools，见原 codex-app-tools.mjs 头注释）。

import (
	_ "embed"
	"encoding/json"
)

//go:embed codex_app_tools.json
var codexAppToolsJSON []byte

// appNamespaces 是快照里 namespace 名 → 工具名 → 定义。
var appNamespaces map[string]map[string]map[string]any

func init() {
	var entries []map[string]any
	if err := json.Unmarshal(codexAppToolsJSON, &entries); err != nil {
		// 快照是构建时校验过的静态数据；解析失败是程序错误，panic
		// 在进程启动时暴露而不是每个请求上。
		panic("codex_app_tools.json: " + err.Error())
	}
	appNamespaces = map[string]map[string]map[string]any{}
	for _, entry := range entries {
		if entry["type"] != "namespace" {
			continue
		}
		name, _ := entry["name"].(string)
		if name == "" {
			continue
		}
		byName := map[string]map[string]any{}
		children, _ := entry["tools"].([]any)
		for _, raw := range children {
			fn, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if childName, _ := fn["name"].(string); childName != "" {
				byName[childName] = fn
			}
		}
		if len(byName) > 0 {
			appNamespaces[name] = byName
		}
	}
}

// MergeCodexAppTools 把完整 app 工具快照合并进请求的工具列表。
// 客户端已发的定义优先；快照只补 deferLoading 推迟的部分；
// namespace 整个缺失时追加完整快照（客户端仍然原生执行这些调用）。
// 返回合并后的列表与是否发生过合并。
func MergeCodexAppTools(tools any) ([]any, bool) {
	list, ok := tools.([]any)
	if !ok || len(appNamespaces) == 0 {
		return nil, false
	}
	merged := make([]any, 0, len(list)+len(appNamespaces))
	changed := false
	seenNamespaces := map[string]bool{}
	for _, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok || tool["type"] != "namespace" {
			merged = append(merged, raw)
			continue
		}
		name, _ := tool["name"].(string)
		full, has := appNamespaces[name]
		if !has {
			merged = append(merged, raw)
			continue
		}
		seen := map[string]bool{}
		clientTools := []any{}
		children, _ := tool["tools"].([]any)
		for _, childRaw := range children {
			fn, ok := childRaw.(map[string]any)
			if !ok {
				continue
			}
			childName, _ := fn["name"].(string)
			if childName == "" {
				continue
			}
			clientTools = append(clientTools, fn)
			seen[childName] = true
		}
		// 快照补客户端 deferLoading 推迟的工具。
		for childName, fn := range full {
			if !seen[childName] {
				clientTools = append(clientTools, fn)
				changed = true
			}
		}
		next := shallowCopy(tool)
		next["tools"] = clientTools
		merged = append(merged, next)
		seenNamespaces[name] = true
	}
	// namespace 整个缺失：追加完整快照。
	for name, byName := range appNamespaces {
		if seenNamespaces[name] {
			continue
		}
		children := make([]any, 0, len(byName))
		for _, fn := range byName {
			children = append(children, fn)
		}
		merged = append(merged, map[string]any{
			"type":        "namespace",
			"name":        name,
			"description": "Tools provided by the Codex app.",
			"tools":       children,
		})
		changed = true
	}
	return merged, changed
}
