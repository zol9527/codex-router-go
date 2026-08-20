package vision

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Engine 是一个可读图的引擎（native GPT：调用方会话里的视觉模型）。
type Engine struct {
	Slug          string
	DisplayName   string
	GatewayModel  string // native 引擎 = slug
	Native        bool
	Priority      int
	Efforts       []string
	DefaultEffort string
	// InputModalities 里含 "image" 是参与候选的先决条件。
	ImageCapable bool
}

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

// ResolveEngines 解析读图引擎：候选按优先级排序取首位。引擎固定为
// native（调用方的 ChatGPT 会话）—— 读图不消耗任何付费 provider
// 配额，也不做同步凭据探测。桥关闭或无候选返回空表。
func ResolveEngines(candidates []Engine, settings Settings, configured bool) []Engine {
	if !settings.EffectiveEnabled(configured) {
		return nil
	}
	ranked := RankVisionEngines(candidates)
	if len(ranked) == 0 {
		return nil
	}
	return []Engine{ranked[0]}
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
