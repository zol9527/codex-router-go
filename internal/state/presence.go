package state

// presence 状态，移植自 presence-state.mjs。
//
// tray 可以把 router 绑定到 Codex/ChatGPT 桌面 app（follow 模式：
// app 全关后 30 秒停服务）。但 follow 只能看 app bundle ——
// NSRunningApplication 枚举不到终端里的 `codex` TUI。一个 tray 看不见
// 的客户端把 router 钉在 always：找不到 4202 端口的回合当场失败，
// 而那背后的栈要最长 300 秒才能热起来，请求延迟里不存在懒启动。
//
// dsh 集成已随本 fork 砍掉，剩下的覆盖信号只有一个：codex 在 PATH 上。
// 检测宁可误报（假阳性只是个沉睡的开关，假阴性是丢掉下一次请求）。

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Presence 模式常量。
const (
	PresenceAlways      = "always"
	PresenceFollowCodex = "follow-codex"
)

// ValidPresenceModes 是合法模式集。
var ValidPresenceModes = []string{PresenceAlways, PresenceFollowCodex}

// ReadPresenceMode 读取存储的模式（tray 的开关；缺失/损坏回落 always）。
func ReadPresenceMode(stateDir string) string {
	raw, err := os.ReadFile(filepath.Join(stateDir, "presence.json"))
	if err != nil {
		return PresenceAlways
	}
	var parsed struct {
		Version int    `json:"version"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Version != 1 {
		return PresenceAlways
	}
	for _, mode := range ValidPresenceModes {
		if parsed.Mode == mode {
			return parsed.Mode
		}
	}
	return PresenceAlways
}

// SetPresenceMode 写入模式（原子替换，0600）。
func SetPresenceMode(stateDir, mode string) error {
	valid := false
	for _, candidate := range ValidPresenceModes {
		if candidate == mode {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("presence mode must be one of: " + strings.Join(ValidPresenceModes, ", "))
	}
	raw, err := json.MarshalIndent(map[string]any{"version": 1, "mode": mode}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, "presence.json")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// TerminalCodexInstalled：codex 可在 shell 里敲出来（桌面 app 的 bundle
// tray 能看见，PATH 上的 codex 它看不见 —— 那是终端会话）。
// 按 PATH 记忆化（答案只在 PATH 变化时改变）。
var terminalCodexMu sync.Mutex
var terminalCodexCache = struct {
	path  string
	found bool
}{}

func TerminalCodexInstalled() bool {
	terminalCodexMu.Lock()
	defer terminalCodexMu.Unlock()
	current := os.Getenv("PATH")
	if terminalCodexCache.path != current {
		terminalCodexCache.path = current
		terminalCodexCache.found = codexOnPath(current)
	}
	return terminalCodexCache.found
}

func codexOnPath(pathValue string) bool {
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "codex")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return true
		}
		// Windows 形态（本 fork 不支持，留作完整性）。
		if info, err := os.Stat(candidate + ".exe"); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// EffectivePresenceMode 是 tray 与 doctor 实际该依据的模式：
// follow 模式下，任何 tray 看不见的客户端（PATH 上的 codex）把实际
// 模式钉在 always。任何要停服务的地方读这个，不读原始模式。
// 存储的模式只被覆盖、从不改写 —— 移除客户端后用户自己的选择
// 在下一次读取时自然恢复。
func EffectivePresenceMode(stateDir string) string {
	mode := ReadPresenceMode(stateDir)
	if mode != PresenceFollowCodex {
		return mode
	}
	if TerminalCodexInstalled() {
		return PresenceAlways
	}
	return mode
}

// ServiceFollowsHostApps 报告 router 是否允许随桌面 app 关闭而停止。
func ServiceFollowsHostApps(stateDir string) bool {
	return EffectivePresenceMode(stateDir) == PresenceFollowCodex
}

// PresenceSnapshot 是 control --json 的 presence 块。tray 的
// RouterPresence 四个字段全部非可选 —— harnessPublished 随 dsh 目标
// 砍掉后仍须发显式 false，缺键会让 tray 整个快照解码失败。
func PresenceSnapshot(stateDir string) map[string]any {
	return map[string]any{
		"mode":             ReadPresenceMode(stateDir),
		"effectiveMode":    EffectivePresenceMode(stateDir),
		"terminalCodex":    TerminalCodexInstalled(),
		"harnessPublished": false,
	}
}

var _ = exec.Command // 保留引用占位（检测用 Stat 而非 spawn，零成本）
