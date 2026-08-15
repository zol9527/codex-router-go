// Package catalog 的子代理（Codex 协作 v2）支持。
//
// 语义移植自 src/multi-agent-state.mjs 与 src/codex-agent-catalog.mjs：
//
//   - ApplySubagentDemotions：disabled 列表里的路由模型降回 v1 ——
//     本地状态只能收窄注册表的证明集合，永远不能放大
//   - PromoteNativeMultiAgent：原生条目按模式提升。上游把 gpt-5.6-luna
//     静态标成 v1 但它实际跑在 v2 后端（spawn_agent 按静态值过滤候选，
//     v1 条目永远不能被 v2 父代理委派）—— 白名单无条件提升；其余
//     原生模型只在 all / selected+加选 时提升
//   - SubagentEligibleModels：注册表声明 v2 且未被关掉的模型
//   - SyncRoutedCodexAgents：把合格模型写成 Codex agents 目录里的
//     router-model-<slug>.toml（按名字可 spawn），并删掉不再合格的
//     受管文件。只认 router-model-* 前缀 —— 目录里用户自己的东西
//     永不被读、被删
package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// nativeV2BackendSlugs：上游已验证跑在 v2 后端却被静态标 v1 的条目，
// 无条件提升 —— 不经模式开关，不经设置页。
var nativeV2BackendSlugs = map[string]bool{"gpt-5.6-luna": true}

// ApplySubagentDemotions 把 disabled / picker 隐藏的路由模型降回 v1
// （复制不改原）。隐藏即降权与 Node 版一致：从 picker 拿掉的模型
// 不该再作为分身候选出现。
func ApplySubagentDemotions(models []*registry.Model, settings state.SubagentSettings, hidden map[string]bool) []*registry.Model {
	disabled := map[string]bool{}
	for _, slug := range settings.Disabled {
		disabled[slug] = true
	}
	out := make([]*registry.Model, 0, len(models))
	for _, m := range models {
		if disabled[m.Slug] || hidden[m.Slug] {
			demoted := *m
			demoted.MultiAgentVersion = "v1"
			out = append(out, &demoted)
			continue
		}
		out = append(out, m)
	}
	return out
}

// PromoteNativeMultiAgent 对原生条目按模式提升 multi_agent_version。
// 只动 visibility=list 且未被本地关掉的条目。
func PromoteNativeMultiAgent(native []NativeModel, settings state.SubagentSettings) []NativeModel {
	enabled := map[string]bool{}
	for _, slug := range settings.Enabled {
		enabled[slug] = true
	}
	disabled := map[string]bool{}
	for _, slug := range settings.Disabled {
		disabled[slug] = true
	}
	out := make([]NativeModel, 0, len(native))
	for _, entry := range native {
		slug, _ := entry["slug"].(string)
		visibility, _ := entry["visibility"].(string)
		promote := visibility == "list" && !disabled[slug] && (
			nativeV2BackendSlugs[slug] ||
				settings.Mode == state.SubagentModeAll ||
				(settings.Mode == state.SubagentModeSelected && enabled[slug]))
		if !promote {
			out = append(out, entry)
			continue
		}
		next := NativeModel{}
		for k, v := range entry {
			next[k] = v
		}
		next["multi_agent_version"] = "v2"
		out = append(out, next)
	}
	return out
}

// SubagentEligibleModels：注册表声明 v2 且未被本地关掉的模型。
func SubagentEligibleModels(models []*registry.Model, settings state.SubagentSettings) []*registry.Model {
	disabled := map[string]bool{}
	for _, slug := range settings.Disabled {
		disabled[slug] = true
	}
	out := []*registry.Model{}
	for _, m := range models {
		if m.MultiAgentVersion == "v2" && !disabled[m.Slug] {
			out = append(out, m)
		}
	}
	return out
}

var managedAgentFile = regexp.MustCompile(`^router-model-[a-z0-9-]+\.toml$`)

func safeIdentifier(value, separator string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteString(separator)
		}
	}
	return strings.Trim(sb.String(), separator)
}

func tomlString(value string) string {
	// JSON 字符串是合法 TOML basic string（引号/转义规则一致）。
	raw, _ := json.Marshal(value)
	return string(raw)
}

// RoutedAgentDefinition 生成一个路由模型的 agent 定义文件内容。
// model_provider 指向 config.toml 里路由器已发布的标记块。
func RoutedAgentDefinition(m *registry.Model) (agentName, fileName, contents string, err error) {
	slug := strings.TrimSpace(m.Slug)
	if slug == "" || !strings.Contains(slug, "/") {
		return "", "", "", fmt.Errorf("cannot create a routed agent for invalid model slug: %q", slug)
	}
	fileStem := "router-model-" + safeIdentifier(slug, "-")
	agentName = "router_" + safeIdentifier(slug, "_")
	displayName := strings.TrimSpace(m.DisplayName)
	if displayName == "" {
		displayName = slug
	}
	contents = strings.Join([]string{
		"# Managed by Codex Router. Refresh the model catalog to update this file.",
		fmt.Sprintf("name = %s", tomlString(agentName)),
		fmt.Sprintf("description = %s", tomlString(displayName+" agent routed through an authenticated Codex Router provider.")),
		`model_provider = "codex-router"`,
		fmt.Sprintf("model = %s", tomlString(slug)),
		"",
		`developer_instructions = """`,
		"Complete the bounded task assigned by the parent agent.",
		"Respect repository instructions, keep changes surgical, and run relevant verification.",
		"For inspection or review claims, cite the exact file and line. Before claiming that something is absent, search the relevant names and paths; before finishing, reopen every cited location and drop any claim that does not hold.",
		"Use only tool names, agent types, and model overrides offered by the current tool schema. Never invent or reuse a stale name; omit an optional override when no offered value fits.",
		"Do not stop after merely announcing a next action. Execute it when it is within scope, or report the exact blocker or decision needed.",
		"Return a concise summary of work completed, checks run, and remaining risks.",
		`"""`,
		"",
	}, "\n")
	return agentName, fileStem + ".toml", contents, nil
}

// SyncResult 记录 agents 目录同步了什么。
type SyncResult struct {
	Written []string // 写入的文件名
	Removed []string // 删除的不再合格的受管文件
}

// SyncRoutedCodexAgents 同步 Codex agents 目录：一个合格模型一份定义；
// 不再合格的模型的定义必须删 —— Codex 按名字提供目录里每个文件，
// 留着的定义意味着设置已关掉、按名字 spawn 却照样应答。
// 失败回滚到同步前的精确现场，因为半同步状态比失败更糟。
func SyncRoutedCodexAgents(models []*registry.Model, agentsDir string) (SyncResult, error) {
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		return SyncResult{}, err
	}
	entries, _ := os.ReadDir(agentsDir)
	var managed []string
	for _, entry := range entries {
		if managedAgentFile.MatchString(entry.Name()) {
			managed = append(managed, entry.Name())
		}
	}
	previous := map[string]string{}
	for _, name := range managed {
		if raw, err := os.ReadFile(filepath.Join(agentsDir, name)); err == nil {
			previous[name] = string(raw)
		}
	}

	// 回滚 = 精确还原同步前状态：受管文件全删，再把之前存在的重写回。
	rollback := func() {
		for _, name := range managed {
			os.Remove(filepath.Join(agentsDir, name))
		}
		for name, raw := range previous {
			os.WriteFile(filepath.Join(agentsDir, name), []byte(raw), 0o600)
		}
	}

	result := SyncResult{Written: []string{}, Removed: []string{}}
	keep := map[string]bool{}
	for _, m := range models {
		_, fileName, contents, err := RoutedAgentDefinition(m)
		if err != nil {
			rollback()
			return SyncResult{}, err
		}
		target := filepath.Join(agentsDir, fileName)
		tmp := target + ".tmp"
		if err := os.WriteFile(tmp, []byte(contents), 0o600); err != nil {
			rollback()
			return SyncResult{}, err
		}
		if err := os.Chmod(tmp, 0o600); err != nil {
			os.Remove(tmp)
			rollback()
			return SyncResult{}, err
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			rollback()
			return SyncResult{}, err
		}
		keep[fileName] = true
		result.Written = append(result.Written, fileName)
	}
	for _, name := range managed {
		if keep[name] {
			continue
		}
		// 删不掉的定义交给 doctor 报告，不阻塞 catalog 写入。
		if err := os.Remove(filepath.Join(agentsDir, name)); err == nil {
			result.Removed = append(result.Removed, name)
		}
	}
	return result, nil
}
