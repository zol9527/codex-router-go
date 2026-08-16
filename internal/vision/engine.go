package vision

import (
	"encoding/json"
	"os"
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

// ResolveEngines 解析读图引擎列表：操作者 pin 优先；auto 只从
// candidates 排名里取第一个非 loopback；pin 失效时——
//   - 操作者显式 pin 的失效是操作者可见的问题（返回空表），
//   - 默认引擎失效（无人选择过）静默落到排名首位。
//
// Router 只选一个引擎，不对同一图片自动切换 provider；失败由 Codex
// 决定是否重试或改选模型。
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
	return []Engine{*primary}
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

// NativeEnginesFromCatalogFile 从 merged-models.json 提取 listed 的
// 视觉原生模型作为引擎候选（server 读图候选与 control 快照的
// nativeEngines 列表共用）。exclude 是要排除的 slug 集合（调用方
// 传 registry slug，防止路由条目被当成原生引擎）。
// 文件缺失/损坏返回 nil —— 引擎候选是增强项，不是启动前提。
func NativeEnginesFromCatalogFile(path string, exclude map[string]bool) []Engine {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var parsed struct {
		Models []struct {
			Slug            string `json:"slug"`
			DisplayName     string `json:"display_name"`
			Priority        any    `json:"priority"`
			Visibility      string `json:"visibility"`
			// 目录里 modality 有两种形态：原生条目是数组
			// （["text","image"]），路由条目构造时也写数组；但历史
			// 上出现过字符串形态，两种都兼容。
			InputModalities any `json:"input_modalities"`
			Efforts         []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			DefaultEffort string `json:"default_reasoning_level"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	var engines []Engine
	for _, model := range parsed.Models {
		if model.Visibility != "list" || exclude[model.Slug] {
			continue
		}
		if !SupportsImage(modalityStrings(model.InputModalities)) {
			continue
		}
		priority := 999
		if p, ok := model.Priority.(float64); ok {
			priority = int(p)
		}
		efforts := make([]string, 0, len(model.Efforts))
		for _, level := range model.Efforts {
			efforts = append(efforts, level.Effort)
		}
		engines = append(engines, Engine{
			Slug: model.Slug, DisplayName: model.DisplayName,
			GatewayModel: model.Slug, Native: true,
			Priority: priority, Efforts: efforts, DefaultEffort: model.DefaultEffort,
			ImageCapable: true,
		})
	}
	return engines
}

// modalityStrings 把目录里的 modality 字段归一成字符串切片：
// 数组取各元素，字符串按空格/逗号切分。
func modalityStrings(field any) []string {
	switch value := field.(type) {
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.FieldsFunc(value, func(r rune) bool {
			return r == ' ' || r == ','
		})
	default:
		return nil
	}
}
