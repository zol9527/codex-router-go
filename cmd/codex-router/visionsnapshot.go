package main

// control --json 的 visionBridge 区块：tray 设置页视觉卡的数据源。
// 形状以 Swift 端 VisionBridgeSnapshot 解码器为准（enabled/
// resolvedEngine/resolvedEngineName/effort）。enabled 非可选，其余
// 字段无值时省略（Swift 侧按可选解码）。

import (
	"path/filepath"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/state"
	"github.com/loyd/codex-router/internal/domain/vision"
)

// visionBridgeSnapshot 构建视觉桥状态块。reg 允许 nil
// （测试/无 registry 语境，此时只剩 native 候选）。
func visionBridgeSnapshot(st *state.State, reg *registry.Registry) map[string]any {
	settings, configured := vision.ReadSettings(st.Dir)
	resolved := vision.ResolveEngines(visionEngineOptions(st, reg), settings, configured)

	snapshot := map[string]any{
		"enabled": settings.EffectiveEnabled(configured),
	}
	if len(resolved) > 0 {
		snapshot["resolvedEngine"] = resolved[0].Slug
		name := resolved[0].DisplayName
		if name == "" {
			name = resolved[0].Slug
		}
		snapshot["resolvedEngineName"] = name
	}
	if settings.Effort != "" {
		snapshot["effort"] = settings.Effort
	}
	return snapshot
}

// visionEngineOptions 返回读图候选（native only），与 serve 进程的
// visionCandidates 同规则：以 merged 目录为准。控制面拿不到活会话，
// native 是否真的够得着由读图时的会话头决定。
func visionEngineOptions(st *state.State, reg *registry.Registry) []vision.Engine {
	exclude := map[string]bool{}
	if reg != nil {
		for _, model := range reg.Models {
			exclude[model.Slug] = true
		}
	}
	return vision.NativeEnginesFromCatalogFile(
		filepath.Join(st.Dir, "merged-models.json"), exclude)
}
