// Package state 的多代理子代理设置：控制哪些已证明（注册表 multiAgentVersion
// "v2"）的模型对 Codex 协作开放为分身。语义逐条移植自 Node 版
// multi-agent-state.mjs：
//
//   - 三种模式：proven（默认，只放开注册表证明过的）/ selected（proven
//     ∪ enabled 手动加选）/ all（全部提升，激进）
//   - 本地状态只能收窄证明集合（disabled 降回 v1），永远不能给未证明
//     的模型制造 v2 —— 证明只属于仓库
//   - 写入原子（tmp+rename，0600），目录 0700
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// 子代理模式集合。
const (
	SubagentModeProven   = "proven"
	SubagentModeSelected = "selected"
	SubagentModeAll      = "all"
)

// SubagentSettings 是 multi-agent-settings.json 的形状。
// Declared 是本地 v2 声明（用户主权通道）：把"注册表未证明"的路由
// 模型提为 v2 分身候选 —— 自己的机器自己证明。disabled / hidden 仍然
// 永远压过声明。
type SubagentSettings struct {
	Version  int      `json:"version"`
	Mode     string   `json:"mode"`
	Enabled  []string `json:"enabled"`
	Disabled []string `json:"disabled"`
	Declared []string `json:"declared"`
}

func validSubagentMode(mode string) bool {
	return mode == SubagentModeProven || mode == SubagentModeSelected || mode == SubagentModeAll
}

func defaultSubagentSettings() SubagentSettings {
	return SubagentSettings{Version: 2, Mode: SubagentModeProven, Enabled: []string{}, Disabled: []string{}, Declared: []string{}}
}

// legacySubagentSettings 迁移第一版的 bool 全局开关（multi-agent-all.json）。
func legacySubagentSettings(stateDir string) (SubagentSettings, bool) {
	raw, err := os.ReadFile(filepath.Join(stateDir, "multi-agent-all.json"))
	if err != nil {
		return SubagentSettings{}, false
	}
	var parsed struct {
		Version int  `json:"version"`
		Enabled bool `json:"enabled"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Version != 1 {
		return SubagentSettings{}, false
	}
	// 损坏的旧状态忽略 —— 保守默认更安全。
	settings := defaultSubagentSettings()
	if parsed.Enabled {
		settings.Mode = SubagentModeAll
	}
	return settings, true
}

func subagentSettingsPath(stateDir string) string {
	return filepath.Join(stateDir, "multi-agent-settings.json")
}

// ReadSubagentSettings 读取设置；坏文件回落旧开关，再回落保守默认。
func ReadSubagentSettings(stateDir string) SubagentSettings {
	raw, err := os.ReadFile(subagentSettingsPath(stateDir))
	if err == nil {
		var parsed SubagentSettings
		if json.Unmarshal(raw, &parsed) == nil &&
			parsed.Version == 2 && validSubagentMode(parsed.Mode) &&
			parsed.Enabled != nil && parsed.Disabled != nil {
			// 旧版文件没有 declared 字段 —— 回填空集而不是拒读
			// （拒读会把用户的 mode/all 静默重置回 proven）。
			if parsed.Declared == nil {
				parsed.Declared = []string{}
			}
			return parsed
		}
	}
	if legacy, ok := legacySubagentSettings(stateDir); ok {
		return legacy
	}
	return defaultSubagentSettings()
}

// WriteSubagentSettings 原子写（tmp+rename，0600；目录确保 0700）。
func WriteSubagentSettings(stateDir string, settings SubagentSettings) error {
	if settings.Version != 2 || !validSubagentMode(settings.Mode) {
		return os.ErrInvalid
	}
	sort.Strings(settings.Enabled)
	sort.Strings(settings.Disabled)
	if settings.Declared == nil {
		settings.Declared = []string{}
	}
	sort.Strings(settings.Declared)
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp := subagentSettingsPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, subagentSettingsPath(stateDir))
}

// SetSubagentMode 切换模式；all 清空 disabled（与 Node 版一致）。
func SetSubagentMode(stateDir, mode string) (SubagentSettings, error) {
	if !validSubagentMode(mode) {
		return SubagentSettings{}, os.ErrInvalid
	}
	current := ReadSubagentSettings(stateDir)
	next := current
	next.Version = 2
	next.Mode = mode
	if mode == SubagentModeAll {
		next.Disabled = []string{}
	}
	err := WriteSubagentSettings(stateDir, next)
	return next, err
}

// SetSubagentModels 批量启停单模型；provider 尺寸的一次性写 —— 比
// 每模型一条命令快，也避免 UI 关闭时留下半个 provider 被清空的状态。
// 在 proven 模式下加选单模型会切到 selected（这是"加选"的语义）。
func SetSubagentModels(stateDir string, slugs []string, enabled bool) (SubagentSettings, error) {
	unique := map[string]bool{}
	for _, slug := range slugs {
		if trimmed := trimNonEmpty(slug); trimmed != "" {
			unique[trimmed] = true
		}
	}
	if len(unique) == 0 {
		return SubagentSettings{}, os.ErrInvalid
	}
	current := ReadSubagentSettings(stateDir)
	enabledSet := toSet(current.Enabled)
	disabledSet := toSet(current.Disabled)
	for slug := range unique {
		if enabled {
			enabledSet[slug] = true
			delete(disabledSet, slug)
		} else {
			delete(enabledSet, slug)
			disabledSet[slug] = true
		}
	}
	mode := current.Mode
	if current.Mode == SubagentModeProven && enabled {
		mode = SubagentModeSelected
	}
	next := SubagentSettings{
		Version:  2,
		Mode:     mode,
		Enabled:  fromSet(enabledSet),
		Disabled: fromSet(disabledSet),
		Declared: current.Declared,
	}
	err := WriteSubagentSettings(stateDir, next)
	return next, err
}

// SetSubagentDeclared 批量增删本地 v2 声明（declare/undeclare 通道）。
// 与 enabled/disabled 正交：声明负责"可当分身"，disabled/hidden 负责
// "一票否决"。
func SetSubagentDeclared(stateDir string, slugs []string, declared bool) (SubagentSettings, error) {
	unique := map[string]bool{}
	for _, slug := range slugs {
		if trimmed := trimNonEmpty(slug); trimmed != "" {
			unique[trimmed] = true
		}
	}
	if len(unique) == 0 {
		return SubagentSettings{}, os.ErrInvalid
	}
	current := ReadSubagentSettings(stateDir)
	declaredSet := toSet(current.Declared)
	for slug := range unique {
		if declared {
			declaredSet[slug] = true
		} else {
			delete(declaredSet, slug)
		}
	}
	next := current
	next.Declared = fromSet(declaredSet)
	err := WriteSubagentSettings(stateDir, next)
	return next, err
}

// SubagentSettingsSnapshot 是 control --json 的 subagents 块。
func SubagentSettingsSnapshot(stateDir string) map[string]any {
	settings := ReadSubagentSettings(stateDir)
	return map[string]any{
		"version":  settings.Version,
		"mode":     settings.Mode,
		"enabled":  settings.Enabled,
		"disabled": settings.Disabled,
		"declared": settings.Declared,
		"all":      settings.Mode == SubagentModeAll,
		"path":     subagentSettingsPath(stateDir),
	}
}

func trimNonEmpty(value string) string {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	return value
}

func toSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, v := range values {
		set[v] = true
	}
	return set
}

func fromSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ReplaceSubagentSettings 整体替换（select-all / unselect-all 用）。
func ReplaceSubagentSettings(stateDir string, settings SubagentSettings) (SubagentSettings, error) {
	if !validSubagentMode(settings.Mode) {
		return SubagentSettings{}, os.ErrInvalid
	}
	settings.Version = 2
	if settings.Enabled == nil {
		settings.Enabled = []string{}
	}
	if settings.Disabled == nil {
		settings.Disabled = []string{}
	}
	err := WriteSubagentSettings(stateDir, settings)
	return settings, err
}
