package vision

import (
	"sort"
	"strings"
)

// Engine 是一个可读图的引擎（注册表模型 / native GPT / 本地 Ollama）。
type Engine struct {
	Slug          string
	DisplayName   string
	GatewayModel  string // registry 引擎的 chat 上游 id；native 引擎 = slug
	Provider      string // registry 引擎的 provider id；native/local 为空或特指
	Native        bool
	Local         bool
	Priority      int
	Efforts       []string
	DefaultEffort string
	// InputModalities 里含 "image" 是参与候选的先决条件。
	ImageCapable bool
}

// Loopback 判定：本地引擎（Ollama/LM Studio）只在操作者显式 pin 时
// 才可当引擎 —— auto 绝不提名 loopback（服务没开就会让每次贴图失败）。
func (e Engine) Loopback() bool { return e.Local }

// SupportsImage 按 inputModalities 声明判定。
func SupportsImage(modalities []string) bool {
	for _, m := range modalities {
		if strings.EqualFold(m, "image") {
			return true
		}
	}
	return false
}

// RankVisionEngines 按优先级排序视觉候选（priority 小者优先）。
func RankVisionEngines(engines []Engine) []Engine {
	ranked := make([]Engine, len(engines))
	copy(ranked, engines)
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].Priority < ranked[j].Priority
	})
	return ranked
}

// MaxEngineAttempts 是一张图最多被送到的引擎数：
// 第二意见值一点配额，第五个不值。
const MaxEngineAttempts = 3

// ResolveEngines 解析读图引擎列表：操作者 pin 优先；auto 只从
// candidates 排名里取第一个非 loopback；pin 失效时——
//   - 操作者显式 pin 的失效是操作者可见的问题（返回空表），
//   - 默认引擎失效（无人选择过）静默落到排名首位。
//
// 首引擎之后按排名补充备用（非 loopback、去重、最多 3 个）：
// 解析得到引擎与"够得着它"是两个问题 —— 401/503 不该让每次贴图
// 都降级成"无法读取"。回退仅对非本地首引擎展开：pin 了本地引擎
// 就是点名自己的机器，绝不为它花没人选择的 provider 配额。
func ResolveEngines(candidates []Engine, settings Settings, configured bool) []Engine {
	if !settings.EffectiveEnabled(configured) {
		return nil
	}
	if settings.Engine == LocalEngineSlug {
		return []Engine{localEngine(settings)}
	}
	ranked := RankVisionEngines(candidates)
	var primary *Engine
	if settings.Engine != "" {
		for i := range ranked {
			if ranked[i].Slug == settings.Engine {
				primary = &ranked[i]
				break
			}
		}
		if primary == nil && !settings.Defaulted {
			return nil // 显式 pin 失效：操作者可见，不静默换模型
		}
	}
	if primary == nil {
		for i := range ranked {
			if !ranked[i].Loopback() {
				primary = &ranked[i]
				break
			}
		}
	}
	if primary == nil {
		return nil
	}
	if primary.Local {
		return []Engine{*primary}
	}
	engines := []Engine{*primary}
	seen := map[string]bool{primary.Slug: true}
	for _, candidate := range ranked {
		if len(engines) >= MaxEngineAttempts {
			break
		}
		if seen[candidate.Slug] || candidate.Loopback() {
			continue
		}
		seen[candidate.Slug] = true
		engines = append(engines, candidate)
	}
	return engines
}

// localEngine 由 pin 设置构造本地引擎（无凭据直连 Ollama 兼容端点）。
func localEngine(settings Settings) Engine {
	baseURL := settings.LocalBaseURL
	if baseURL == "" {
		baseURL = DefaultLocalVisionBaseURL
	}
	model := settings.LocalModel
	if model == "" {
		model = DefaultLocalVisionModel
	}
	return Engine{
		Slug:         LocalEngineSlug,
		DisplayName:  "local (" + model + ")",
		GatewayModel: model,
		Local:        true,
		ImageCapable: true,
		Priority:     999,
	}
}

// LocalBaseURLOf 从 settings 解出本地端点。
func LocalBaseURLOf(settings Settings) string {
	if settings.LocalBaseURL != "" {
		return settings.LocalBaseURL
	}
	return DefaultLocalVisionBaseURL
}

// LocalModelOf 从 settings 解出本地模型。
func LocalModelOf(settings Settings) string {
	if settings.LocalModel != "" {
		return settings.LocalModel
	}
	return DefaultLocalVisionModel
}
