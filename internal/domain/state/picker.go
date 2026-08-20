// Package state 的模型选择器可见性：hidden 列表里的模型从 Codex
// picker 目录中拿掉（仍可按名路由 —— 只是不可见）。语义对应 Node 版
// model-picker-state.mjs：隐藏不是删除，显示回来即恢复。
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

type pickerState struct {
	Version int      `json:"version"`
	Hidden  []string `json:"hidden"`
}

func pickerStatePath(stateDir string) string {
	return filepath.Join(stateDir, "model-picker.json")
}

// ReadPickerHidden 返回隐藏 slug 集合（空文件/坏文件 = 无隐藏）。
func ReadPickerHidden(stateDir string) map[string]bool {
	raw, err := os.ReadFile(pickerStatePath(stateDir))
	if err != nil {
		return map[string]bool{}
	}
	var parsed pickerState
	if json.Unmarshal(raw, &parsed) != nil {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, slug := range parsed.Hidden {
		set[slug] = true
	}
	return set
}

// SetPickerModels 批量设置可见性；原子写（0600）。
func SetPickerModels(stateDir string, slugs []string, visible bool) error {
	unique := map[string]bool{}
	for _, slug := range slugs {
		if trimmed := trimNonEmpty(slug); trimmed != "" {
			unique[trimmed] = true
		}
	}
	if len(unique) == 0 {
		return os.ErrInvalid
	}
	current := ReadPickerHidden(stateDir)
	for slug := range unique {
		if visible {
			delete(current, slug)
		} else {
			current[slug] = true
		}
	}
	return writePickerHidden(stateDir, current)
}

// ClearPickerHidden 全部显示（picker all show）。
func ClearPickerHidden(stateDir string) error {
	return writePickerHidden(stateDir, map[string]bool{})
}

func writePickerHidden(stateDir string, set map[string]bool) error {
	hidden := make([]string, 0, len(set))
	for slug := range set {
		hidden = append(hidden, slug)
	}
	sort.Strings(hidden)
	raw, err := json.MarshalIndent(pickerState{Version: 1, Hidden: hidden}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp := pickerStatePath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, pickerStatePath(stateDir))
}

// PickerSnapshot 是 control --json 的 picker 块。
func PickerSnapshot(stateDir string) map[string]any {
	set := ReadPickerHidden(stateDir)
	hidden := make([]string, 0, len(set))
	for slug := range set {
		hidden = append(hidden, slug)
	}
	sort.Strings(hidden)
	return map[string]any{"hidden": hidden}
}
