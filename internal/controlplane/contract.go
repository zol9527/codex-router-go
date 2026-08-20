// Package controlplane 拥有 tray 控制面的两样东西：
//
//   - JSON 契约（Snapshot 族）：`control --json` 输出的类型化定义。
//     Swift 端 RouterSnapshot 解码器的字段存在性在此锚定 —— 加字段改
//     这里并同步 Swift；presence 等子块由各 snapshot 生成器注入，
//     顶层形状由本包钉死。历史上 map[string]any 手拼输出曾因漏一个
//     字段让整个 Swift 解码失败、面板落到「路由不可用」（2026-08
//     harnessPublished 事故），类型化后此类漂移在编译期可见。
//   - 服务生命周期（Service）：detached 启动与 pidfile 停止。App 的
//     ServiceSupervisor 子进程托管是另一条生命周期路径，不经这里 ——
//     本包只负责终端救急与 `control service` 命令面。
package controlplane

import (
	"encoding/json"
	"sort"

	"github.com/loyd/codex-router/internal/catalog"
	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// ProviderOrder 是控制面展示 provider 的固定顺序锚点（UI 卡片排序
// 依据，覆盖当前全部内置 provider）。
func ProviderOrder() []string {
	return []string{"zai-coding", "opencode-go", "litellm"}
}

// OrderedProviders 返回控制面展示顺序的 provider。固定名单锚定本 fork
// 默认支持的路由面（zai-coding / opencode-go / litellm）；用户显式启用
// 的白名单外 provider 按字母序追加 —— 注册表还携带 models.dev 生态的
// 参考 provider 定义（28 个家族），未启用的不得涌入设置页。变体跟随
// 家族主项，不单独展示。
func OrderedProviders(reg *registry.Registry, enabled func(id string) bool) []*registry.Provider {
	seen := map[string]bool{}
	var ordered []*registry.Provider
	for _, id := range ProviderOrder() {
		p := reg.Providers[id]
		if p == nil || p.VariantOf != "" {
			continue
		}
		ordered = append(ordered, p)
		seen[id] = true
	}
	var rest []string
	for id, p := range reg.Providers {
		if seen[id] || p == nil || p.VariantOf != "" {
			continue
		}
		if enabled == nil || !enabled(id) {
			continue // 参考定义：用户未启用，不进设置页
		}
		rest = append(rest, id)
	}
	sort.Strings(rest)
	for _, id := range rest {
		ordered = append(ordered, reg.Providers[id])
	}
	return ordered
}

// Snapshot 是 `control --json` 的顶层形状（tray 五分钟轮询的主快照）。
// Presence / ModelSettings 子块以 RawMessage 注入：它们的形状分别归
// state 包与 vision 快照生成器所有，本包只负责挂载位置。
type Snapshot struct {
	Targets  map[string]Target `json:"targets"`
	Version  string            `json:"version"`
	Presence json.RawMessage   `json:"presence"`
}

// Target 是单个路由目标（当前仅 codex）的状态面。
type Target struct {
	Target           string         `json:"target"`
	Configured       bool           `json:"configured"`
	Active           bool           `json:"active"`
	EnabledProviders []string       `json:"enabledProviders"`
	Providers        []ProviderInfo `json:"providers"`
	Models           []ModelInfo    `json:"models"`
	ModelSettings    ModelSettings  `json:"modelSettings"`
}

// ProviderInfo 是设置页 provider 卡片的形状。CredentialConfigured 与
// CredentialSource 是 Go 侧自留的观测字段（Swift 解码器当前忽略，
// 多余字段对 Decodable 无害），保留它们供 CLI 人读。
type ProviderInfo struct {
	ID                   string `json:"id"`
	DisplayName          string `json:"displayName"`
	Kind                 string `json:"kind"`
	Enabled              bool   `json:"enabled"`
	CredentialConfigured bool   `json:"credentialConfigured"`
	CredentialSource     string `json:"credentialSource"`
}

// ModelInfo 是模型列表条目。Swift 端 RouterModel.enabled 非可选 —— 能进
// 这张表的模型都是已启用 provider 下的已发布模型，恒为 true。字段名即
// 契约：改名的代价是旧 tray 整体解码失败，需两侧同步发布。
type ModelInfo struct {
	Slug              string `json:"slug"`
	DisplayName       string `json:"displayName"`
	Provider          string `json:"provider"`
	Enabled           bool   `json:"enabled"`
	Visible           bool   `json:"visible"`
	Proven            bool   `json:"proven"`
	Declared          bool   `json:"declared"`
	MultiAgentVersion string `json:"multiAgentVersion,omitempty"`
}

// ModelSettings 聚合设置页三个区块的数据源。
type ModelSettings struct {
	Subagents    json.RawMessage `json:"subagents"`
	Picker       json.RawMessage `json:"picker"`
	VisionBridge json.RawMessage `json:"visionBridge"`
}

// SnapshotDeps 是构建快照的外部事实。子快照以 marshaled JSON 注入，
// 保持本包对 usage / vision 快照细节零依赖。
type SnapshotDeps struct {
	State  *state.State
	Reg    *registry.Registry
	Version string
	// Active 是 /health 探测结果（服务是否在跑）。
	Active bool
	// 子快照注入（各自生成器所有）。
	Presence     json.RawMessage
	Subagents    json.RawMessage
	Picker       json.RawMessage
	VisionBridge json.RawMessage
}

// BuildSnapshot 组装 tray 主快照。纯计算：读状态目录与注册表，无网络、
// 无进程副作用（Active 由调用方探测后传入）。
func BuildSnapshot(deps SnapshotDeps) Snapshot {
	enabled := deps.State.EnabledProviders()
	// 空启用集归一为空 slice：nil slice 序列化成 null，Swift [String]
	// 解码 null 直接失败（契约点，测试钉住）。
	if enabled == nil {
		enabled = []string{}
	}
	enabledSet := map[string]bool{}
	for _, id := range enabled {
		enabledSet[id] = true
	}
	resolver := cred.New(deps.State)

	providers := []ProviderInfo{}
	for _, p := range OrderedProviders(deps.Reg, func(id string) bool { return enabledSet[id] }) {
		_, source := resolver.Resolve(p)
		providers = append(providers, ProviderInfo{
			ID: p.ID, DisplayName: p.DisplayName, Kind: p.Kind,
			Enabled:              enabledSet[p.ID],
			CredentialConfigured: source != "",
			CredentialSource:     source,
		})
	}

	hidden := state.ReadPickerHidden(deps.State.Dir)
	// 分身裁决与 catalog 同规：注册表证明 OR 本地声明，disabled/隐藏
	// 一票否决。UI 看到的必须是有效版本而不是注册表原始值 —— 否则
	// 本地声明的模型（declared）在设置页里凭空消失。
	subagentSettings := state.ReadSubagentSettings(deps.State.Dir)
	resolvedSubagents := map[string]*registry.Model{}
	for _, m := range catalog.ApplySubagentMultiAgent(deps.Reg.Models, subagentSettings, hidden) {
		resolvedSubagents[m.Slug] = m
	}
	declaredSubagents := map[string]bool{}
	for _, slug := range subagentSettings.Declared {
		declaredSubagents[slug] = true
	}

	models := []ModelInfo{}
	for _, m := range deps.Reg.Models {
		if !m.Listed || !deps.State.ProviderEnabled(m.Provider, deps.Reg.CanonicalProviderID) {
			continue
		}
		entry := ModelInfo{
			Slug: m.Slug, DisplayName: m.DisplayName,
			Provider: deps.Reg.CanonicalProviderID(m.Provider),
			Enabled:  true,
			// visible = 未被 picker 隐藏；proven/declared 供分身 UI
			// 区分开关语义：证明过的走 disabled 收窄，未证明的走声明通道。
			Visible:  !hidden[m.Slug],
			Proven:   m.MultiAgentVersion == "v2",
			Declared: declaredSubagents[m.Slug],
		}
		if resolved := resolvedSubagents[m.Slug]; resolved != nil && resolved.MultiAgentVersion == "v2" {
			entry.MultiAgentVersion = "v2"
		}
		models = append(models, entry)
	}

	return Snapshot{
		Targets: map[string]Target{"codex": {
			Target:           "codex",
			Configured:       true,
			Active:           deps.Active,
			EnabledProviders: enabled,
			Providers:        providers,
			Models:           models,
			ModelSettings: ModelSettings{
				Subagents:    deps.Subagents,
				Picker:       deps.Picker,
				VisionBridge: deps.VisionBridge,
			},
		}},
		Version:  deps.Version,
		Presence: deps.Presence,
	}
}
