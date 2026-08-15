package main

// control --json 的 visionBridge 区块：tray 设置页视觉卡的数据源。
// 形状以 Swift 端 VisionBridgeSnapshot 解码器为准（enabled/engine/
// local/resolvedEngine/resolvedEngineName/hostMemGib/paidEngines/
// nativeEngines/effort/download）。enabled 非可选，其余字段无值时省略
// （Swift 侧按可选解码）。

import (
	"path/filepath"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
	"github.com/loyd/codex-router/internal/vision"
)

// visionBridgeSnapshot 构建视觉桥状态块。reg 允许 nil
// （测试/无 registry 语境，此时只剩 native 候选）。
func visionBridgeSnapshot(st *state.State, reg *registry.Registry) map[string]any {
	settings, configured := vision.ReadSettings(st.Dir)
	candidates, paidEngines, nativeEngines := visionEngineOptions(st, reg)
	resolved := vision.ResolveEngines(candidates, settings, configured)

	snapshot := map[string]any{
		"enabled": settings.EffectiveEnabled(configured),
	}
	if settings.Engine != "" {
		snapshot["engine"] = settings.Engine
	}
	if settings.Engine == vision.LocalEngineSlug {
		snapshot["local"] = map[string]any{"model": vision.LocalModelOf(settings)}
	}
	if len(resolved) > 0 {
		snapshot["resolvedEngine"] = resolved[0].Slug
		name := resolved[0].DisplayName
		if name == "" {
			name = resolved[0].Slug
		}
		snapshot["resolvedEngineName"] = name
	}
	if mem := hostMemGiB(); mem > 0 {
		snapshot["hostMemGib"] = mem
	}
	if len(paidEngines) > 0 {
		snapshot["paidEngines"] = paidEngines
	}
	if len(nativeEngines) > 0 {
		snapshot["nativeEngines"] = nativeEngines
	}
	if settings.Effort != "" {
		snapshot["effort"] = settings.Effort
	}
	if download := vision.ReadDownload(st.Dir); download != nil {
		snapshot["download"] = download
	}
	return snapshot
}

// visionEngineOptions 返回（解析候选全集, paidEngines, nativeEngines）。
// 候选全集与 serve 进程的 visionCandidates 同规则：registry 模型须
// 已启用 provider 且凭据在位（paidEngines 是它的 UI 投影）。native
// 候选以目录为准：控制面拿不到活会话，是否真的够得着由读图时的
// 会话头决定。
func visionEngineOptions(st *state.State, reg *registry.Registry) (candidates []vision.Engine, paid, native []map[string]any) {
	if reg != nil {
		resolver := cred.New(st)
		for _, model := range reg.Models {
			if !vision.SupportsImage(model.InputModalities) {
				continue
			}
			efforts := make([]string, 0, len(model.ReasoningLevels))
			for _, level := range model.ReasoningLevels {
				efforts = append(efforts, level.Effort)
			}
			provider := reg.Providers[model.Provider]
			if provider == nil || !st.ProviderEnabled(model.Provider, reg.CanonicalProviderID) {
				continue
			}
			if credential, _ := resolver.Resolve(provider); credential == "" {
				continue
			}
			candidates = append(candidates, vision.Engine{
				Slug: model.Slug, DisplayName: model.DisplayName,
				GatewayModel: model.UpstreamModel, Provider: model.Provider,
				Priority: model.Priority, Efforts: efforts,
				DefaultEffort: model.DefaultEffort, ImageCapable: true,
			})
			paid = append(paid, engineOption(model.Slug, model.DisplayName, efforts))
		}
	}
	exclude := map[string]bool{}
	if reg != nil {
		for _, model := range reg.Models {
			exclude[model.Slug] = true
		}
	}
	for _, engine := range vision.NativeEnginesFromCatalogFile(
		filepath.Join(st.Dir, "merged-models.json"), exclude) {
		candidates = append(candidates, engine)
		native = append(native, engineOption(engine.Slug, engine.DisplayName, engine.Efforts))
	}
	return candidates, paid, native
}

func engineOption(slug, displayName string, efforts []string) map[string]any {
	list := make([]any, 0, len(efforts))
	for _, effort := range efforts {
		list = append(list, effort)
	}
	return map[string]any{
		"slug": slug, "displayName": displayName, "efforts": list,
	}
}
