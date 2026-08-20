package main

// vision-bridge 的 control 面板命令。读图引擎固定为 native（调用方的
// ChatGPT 会话），没有引擎选择；这里只剩开关、档位与状态。

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/state"
	"github.com/loyd/codex-router/internal/domain/vision"
)

// controlVisionBridge 处理 control vision-bridge <action>。
// stateDir 由 cmdControl 解析传入（--state 的剥离在那里统一完成）。
// on/off/effort 是 tray 设置页视觉卡的写路径。
func controlVisionBridge(stateDir string, reg *registry.Registry, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("vision-bridge requires on|off|effort|status")
	}
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	action := args[0]
	switch action {
	case "on", "off":
		settings, _ := vision.ReadSettings(st.Dir)
		enabled := action == "on"
		settings.Enabled = &enabled
		if err := vision.WriteSettings(st.Dir, settings); err != nil {
			return err
		}
		return printVisionStatus(st, reg)
	case "effort":
		if len(args) < 2 {
			return fmt.Errorf("effort requires a level or \"default\"")
		}
		settings, configured := vision.ReadSettings(st.Dir)
		materializeDefaultOn(&settings, configured)
		if args[1] == "default" {
			settings.Effort = ""
		} else if args[1] == "" {
			return fmt.Errorf("effort level must not be empty")
		} else {
			settings.Effort = args[1]
		}
		if err := vision.WriteSettings(st.Dir, settings); err != nil {
			return err
		}
		warnVisionWriteState(settings, configured)
		return printVisionStatus(st, reg)
	case "status":
		return printVisionStatus(st, reg)
	default:
		return fmt.Errorf("unknown vision-bridge action %q", action)
	}
}

// materializeDefaultOn 在写第一个配置文件时把默认的 enabled=true 落成
// 显式值：门控语义里"文件存在但 enabled 缺失"按 off 处理，不物化的话
// effort 这类与开关无关的动作会把默认开的桥静默关掉。
// 已配置过的文件不动 —— 存过 false 就是操作者的答案，永远照字面取用。
func materializeDefaultOn(settings *vision.Settings, configured bool) {
	if !configured && settings.Enabled == nil {
		enabled := true
		settings.Enabled = &enabled
	}
}

// warnVisionWriteState 把"写成功但桥不生效"的情形提示到 stderr
// （stdout 保持纯 JSON，tray 页脚只显示命令错误）。
func warnVisionWriteState(settings vision.Settings, configured bool) {
	if configured && settings.Enabled == nil {
		fmt.Fprintln(os.Stderr, "note: previous settings file was unreadable and has been rewritten; the bridge stays off until 'vision-bridge on'")
	}
}

// printVisionStatus 输出与 control --json 同形状的视觉桥状态块，
// 写动作之后操作者（与 tray 的下一次刷新）立即看到生效值。
func printVisionStatus(st *state.State, reg *registry.Registry) error {
	raw, err := json.MarshalIndent(visionBridgeSnapshot(st, reg), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}
