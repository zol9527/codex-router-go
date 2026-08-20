// Package codexconfig 管理 ~/.codex/config.toml 里的路由标记块。
// 只拥有两个块：根级（Responses 路由、模型目录与 Voice 原生端点）与
// provider 表（[model_providers.codex-router]）。其余一切内容 ——
// 用户的 profile、trust、MCP、features —— 一概不碰。
// 用户自有的 openai_base_url / model_catalog_json 是拒绝路径，不是覆盖路径。
package codexconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loyd/codex-router/internal/lib/tomlconf"
)

const (
	startMarker   = "# BEGIN codex-router-managed"
	endMarker     = "# END codex-router-managed"
	providerStart = "# BEGIN codex-router-provider-managed"
	providerEnd   = "# END codex-router-provider-managed"
	providerID    = "codex-router"

	defaultChatGPTBaseURL           = "https://chatgpt.com/backend-api"
	defaultRealtimeWebSocketBaseURL = "https://api.openai.com/v1"
	realtimeCallBaseURLKey          = "experimental_realtime_webrtc_call_base_url"
	realtimeWebSocketBaseURLKey     = "experimental_realtime_ws_base_url"
)

var (
	rootBaseURLPattern    = regexp.MustCompile(`(?m)^\s*openai_base_url\s*=`)
	rootCatalogPattern    = regexp.MustCompile(`(?m)^\s*model_catalog_json\s*=`)
	providerHeaderPattern = regexp.MustCompile(`(?m)^\s*\[model_providers\.` + regexp.QuoteMeta(providerID) + `\]`)
)

// RouterConfig 是要写入的两个块的值。
type RouterConfig struct {
	BaseURL     string // http://127.0.0.1:4202/_codex-router/<secret>/v1
	CatalogPath string // state/merged-models.json 绝对路径
}

// rootBlock 渲染根级标记块。Voice 使用的 WebRTC 会话和侧带 WebSocket
// 不经过 Responses router；只有用户未显式指定时才写入原生端点。
func rootBlock(cfg RouterConfig, addRealtimeCall, addRealtimeWebSocket bool, realtimeCallBaseURL string) string {
	lines := []string{
		startMarker,
		fmt.Sprintf("openai_base_url = %s", tomlconf.Quote(cfg.BaseURL)),
		fmt.Sprintf("model_catalog_json = %s", tomlconf.Quote(cfg.CatalogPath)),
	}
	if addRealtimeCall {
		lines = append(lines, fmt.Sprintf("%s = %s", realtimeCallBaseURLKey, tomlconf.Quote(realtimeCallBaseURL)))
	}
	if addRealtimeWebSocket {
		lines = append(lines, fmt.Sprintf("%s = %s", realtimeWebSocketBaseURLKey, tomlconf.Quote(defaultRealtimeWebSocketBaseURL)))
	}
	lines = append(lines, endMarker)
	return strings.Join(lines, "\n")
}

// providerBlock 渲染 provider 表标记块。
func providerBlock(cfg RouterConfig) string {
	return strings.Join([]string{
		providerStart,
		"[model_providers." + providerID + "]",
		`name = "Codex Router (external models)"`,
		fmt.Sprintf("base_url = %s", tomlconf.Quote(cfg.BaseURL)),
		`wire_api = "responses"`,
		`supports_standalone_web_search = true`,
		providerEnd,
	}, "\n")
}

// removeBlock 抠掉一个标记块（含）与其紧邻的前导空行。
// 漂移保护：若宿主应用（Codex 桌面端重写 config.toml 时）把用户自己的
// 配置包进了标记块，管理块内容只占块头的连续一段；从第一行外来内容起
// 到 END 标记之间的所有行原样保留，绝不能跟着管理块一起被删。
func removeBlock(lines []string, start, end string, isOurs func(string) bool) []string {
	var out []string
	inBlock := false
	sawForeign := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == start {
			inBlock = true
			// 吃掉块前的空行，避免反复启停留下空行堆积。
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			continue
		}
		if inBlock {
			if trimmed == end {
				inBlock = false
				continue
			}
			if !sawForeign && isOurs(trimmed) {
				continue
			}
			sawForeign = true
			out = append(out, line)
			continue
		}
		out = append(out, line)
	}
	return out
}

// rootOwnedLine 判断根级管理块内的一行是否归本路由所有。Realtime 字段仅在
// 值等于 router 会写入的原生默认值时才视为受管，避免吞掉用户手写的覆盖值。
func rootOwnedLine(realtimeCallBaseURL string) func(string) bool {
	return func(trimmed string) bool {
		return trimmed == "" ||
			strings.HasPrefix(trimmed, "openai_base_url =") ||
			strings.HasPrefix(trimmed, "model_catalog_json =") ||
			lineHasTomlValue(trimmed, realtimeCallBaseURLKey, realtimeCallBaseURL) ||
			lineHasTomlValue(trimmed, realtimeWebSocketBaseURLKey, defaultRealtimeWebSocketBaseURL)
	}
}

// providerOwnedLine 判断 provider 管理块内的一行是否归本路由所有。
func providerOwnedLine(trimmed string) bool {
	return trimmed == "" ||
		trimmed == "[model_providers."+providerID+"]" ||
		strings.HasPrefix(trimmed, "name = ") ||
		strings.HasPrefix(trimmed, "base_url = ") ||
		strings.HasPrefix(trimmed, "wire_api = ") ||
		strings.HasPrefix(trimmed, "supports_standalone_web_search = ")
}

// splitManaged 检查非标记块内的冲突字段。
func hasUserOwnedFields(lines []string) (baseURL bool, catalogPath bool) {
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == startMarker || trimmed == providerStart {
			inBlock = true
		}
		if trimmed == endMarker || trimmed == providerEnd {
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		if rootBaseURLPattern.MatchString(line) {
			baseURL = true
		}
		if rootCatalogPattern.MatchString(line) {
			catalogPath = true
		}
	}
	return
}

// Install 写入/更新两个标记块。用户自有的同名字段 → 拒绝并报错。
func Install(configPath string, cfg RouterConfig) error {
	content := ""
	if raw, err := os.ReadFile(configPath); err == nil {
		content = string(raw)
	}
	lines := strings.Split(content, "\n")
	if base, cat := hasUserOwnedFields(lines); base || cat {
		return fmt.Errorf(
			"refusing to replace user-owned %s; remove it or let the router own it explicitly",
			joinNamed(base, cat))
	}
	// 去掉旧管理块再重写（幂等）。
	realtimeCallBaseURL := nativeRealtimeCallBaseURL(lines)
	lines = removeBlock(lines, startMarker, endMarker, rootOwnedLine(realtimeCallBaseURL))
	lines = removeBlock(lines, providerStart, providerEnd, providerOwnedLine)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	// 根级块必须落在第一个 TOML 表头之前（根级字段区）。
	insertAt := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			insertAt = i
			break
		}
	}
	block := strings.Split(rootBlock(
		cfg,
		!hasRootValue(lines, realtimeCallBaseURLKey),
		!hasRootValue(lines, realtimeWebSocketBaseURLKey),
		realtimeCallBaseURL,
	), "\n")
	rest := append([]string{}, lines[insertAt:]...)
	lines = append(lines[:insertAt], block...)
	if len(rest) > 0 {
		lines = append(lines, "")
		lines = append(lines, rest...)
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	lines = append(lines, "", providerBlock(cfg), "")
	next := strings.Join(lines, "\n")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(configPath, []byte(next), 0o600); err != nil {
		return err
	}
	return os.Chmod(configPath, 0o600)
}

// Uninstall 抠掉两个管理块，其余原样保留。
func Uninstall(configPath string) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lines := strings.Split(string(raw), "\n")
	lines = removeBlock(lines, startMarker, endMarker, rootOwnedLine(nativeRealtimeCallBaseURL(lines)))
	lines = removeBlock(lines, providerStart, providerEnd, providerOwnedLine)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	next := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(configPath, []byte(next), 0o600)
}

// nativeRealtimeCallBaseURL 保持 Voice 的会话创建仍然走 ChatGPT 原生
// backend；当用户给 chatgpt_base_url 配了私有网关时，同步从该根路径派生。
func nativeRealtimeCallBaseURL(lines []string) string {
	baseURL := defaultChatGPTBaseURL
	if value, ok := rootValue(lines, "chatgpt_base_url"); ok && value != "" {
		baseURL = value
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/codex") {
		return baseURL
	}
	return baseURL + "/codex"
}

// hasRootValue 只看首个 TOML 表头之前的根级字段，防止把项目或 provider
// 表中的同名字段误当作用户对全局 Voice 端点的覆盖。
func hasRootValue(lines []string, key string) bool {
	_, ok := rootValue(lines, key)
	return ok
}

func rootValue(lines []string, key string) (string, bool) {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return "", false
		}
		if k, v, ok := tomlconf.ParseKeyValue(line); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func lineHasTomlValue(line, key, want string) bool {
	k, v, ok := tomlconf.ParseKeyValue(line)
	return ok && k == key && v == want
}

// Status 报告集成状态（脱敏：不回完整 base URL）。
func Status(configPath string) (installed bool, baseURL string, catalogPath string) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return false, "", ""
	}
	lines := strings.Split(string(raw), "\n")
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == startMarker {
			inBlock = true
			installed = true
		}
		if trimmed == endMarker {
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		if k, v, ok := tomlconf.ParseKeyValue(line); ok && k == "openai_base_url" {
			baseURL = v
		}
		if k, v, ok := tomlconf.ParseKeyValue(line); ok && k == "model_catalog_json" {
			catalogPath = v
		}
	}
	return
}

func joinNamed(base, catalog bool) string {
	var names []string
	if base {
		names = append(names, "openai_base_url")
	}
	if catalog {
		names = append(names, "model_catalog_json")
	}
	return strings.Join(names, " and ")
}

// ProviderTableOwned 检查 provider 表是否归本路由管理
// （doctor 用：区分"我们的表"和"用户自己写的表"）。
func ProviderTableOwned(configPath string) bool {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), providerStart)
}
