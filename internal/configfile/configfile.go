// Package configfile 管理 ~/.codex/config.toml 里的路由标记块。
// 只拥有两个块：根级（openai_base_url / model_catalog_json）与
// provider 表（[model_providers.codex-router]）。其余一切内容 ——
// 用户的 profile、trust、MCP、features —— 一概不碰。
// 用户自有的 openai_base_url / model_catalog_json 是拒绝路径，不是覆盖路径。
package configfile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	startMarker   = "# BEGIN codex-router-managed"
	endMarker     = "# END codex-router-managed"
	providerStart = "# BEGIN codex-router-provider-managed"
	providerEnd   = "# END codex-router-provider-managed"
	providerID    = "codex-router"
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

// rootBlock 渲染根级标记块。
func rootBlock(cfg RouterConfig) string {
	return strings.Join([]string{
		startMarker,
		fmt.Sprintf("openai_base_url = %s", tomlString(cfg.BaseURL)),
		fmt.Sprintf("model_catalog_json = %s", tomlString(cfg.CatalogPath)),
		endMarker,
	}, "\n")
}

// providerBlock 渲染 provider 表标记块。
func providerBlock(cfg RouterConfig) string {
	return strings.Join([]string{
		providerStart,
		"[model_providers." + providerID + "]",
		`name = "Codex Router (external models)"`,
		fmt.Sprintf("base_url = %s", tomlString(cfg.BaseURL)),
		`wire_api = "responses"`,
		`supports_standalone_web_search = true`,
		providerEnd,
	}, "\n")
}

func tomlString(value string) string {
	// JSON 字符串就是合法 TOML 基本字符串（转义规则兼容）。
	return mustJSON(value)
}

func mustJSON(value string) string {
	var buf strings.Builder
	buf.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\t':
			buf.WriteString(`\t`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
	return buf.String()
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

// rootOwnedLine 判断根级管理块内的一行是否归本路由所有。
func rootOwnedLine(trimmed string) bool {
	return trimmed == "" ||
		strings.HasPrefix(trimmed, "openai_base_url =") ||
		strings.HasPrefix(trimmed, "model_catalog_json =")
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
	lines = removeBlock(lines, startMarker, endMarker, rootOwnedLine)
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
	block := strings.Split(rootBlock(cfg), "\n")
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
	lines = removeBlock(lines, startMarker, endMarker, rootOwnedLine)
	lines = removeBlock(lines, providerStart, providerEnd, providerOwnedLine)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	next := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(configPath, []byte(next), 0o600)
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
		if v, ok := tomlValue(line, "openai_base_url"); ok {
			baseURL = v
		}
		if v, ok := tomlValue(line, "model_catalog_json"); ok {
			catalogPath = v
		}
	}
	return
}

func tomlValue(line, key string) (string, bool) {
	pattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=\s*"(.*)"\s*(?:#.*)?$`)
	m := pattern.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
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
