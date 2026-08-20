package main

// control probe：上游行为探针。直接打 provider 端点（绕过 serve 的
// 翻译管道），把"上游当不可信边界"运营化——上游的行为漂移（静默丢
// 字段、计数口径变化、模型名路由调整）无法靠本地测试发现，只能在
// 实机上探测。三项探测全部来自 2026-08-16 的 arguments 丢弃事故的
// 手工诊断法（诊断成本一下午），固化为一条命令：
//
//	control probe zai-coding [model-slug]
//
//   1. models       ：GET /models 对照请求模型名是否存在（当天实锤：
//     glm-5.3[1m] 两协议端点均 1214 拒绝，官方文档与服务端不符）；
//   2. args-visible ：16KB tool_call arguments 埋密码的两形态 needle
//     （原始 tool_calls 形态 vs 镜像进 tool content 形态），判定上游
//     是否丢弃 arguments（镜像补丁 MirrorToolCallArguments 的必要性
//     依据；上游修复后可停用镜像）；
//   3. usage-accounts：阶梯尺寸 arguments（2KB vs 32KB）看 prompt_tokens
//     是否随内容增长，检测计数口径异常（事故时 16KB arguments 计数
//     恒 +10 —— 字段级不计入）。
//
// 全部非流式小请求（总成本 < 2k token）。结果打印可读表格并落盘
// <state>/probe-latest.json（tray/诊断留档）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// probeNeedle 是可见性测试的密码短语（内容任意，固定以便对照档案）。
const probeNeedle = "ZEBRA-42-QUARTZ"

// probeFiller 是参数填充文本：中性英文，确保 token 密度稳定。
const probeFiller = "cache friendliness requires deterministic truncation keyed on content only. "

func controlProbe(st *state.State, reg *registry.Registry, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: control probe <provider-id> [model-slug]")
	}
	provider := reg.Providers[args[0]]
	if provider == nil {
		return fmt.Errorf("unknown provider %q", args[0])
	}
	key, source := cred.New(st).Resolve(provider)
	if key == "" {
		return fmt.Errorf("provider %s has no credential (source: %q)", provider.ID, source)
	}
	model := pickProbeModel(reg, provider, args)
	if model == "" {
		return fmt.Errorf("provider %s has no models registered", provider.ID)
	}
	base := probeBaseURL(st, provider)
	client := &http.Client{Timeout: 60 * time.Second}

	report := map[string]any{
		"provider": provider.ID, "model": model, "baseURL": base,
		"at": time.Now().Format(time.RFC3339),
	}

	// 1. models 列表与模型名存在性。
	modelsOK, listed, err := probeModelsList(client, base, key, model)
	report["modelsListed"] = modelsOK
	report["modelExists"] = err == nil && listed
	if err != nil {
		report["modelsError"] = err.Error()
	}

	// 2. arguments 可见性（两形态）。
	raw, mirror, rawDetail, mirrorDetail, err := probeArgumentsVisibility(client, base, key, model)
	if err != nil {
		report["argsProbeError"] = err.Error()
	} else {
		// raw=true 表示上游不丢 arguments（镜像补丁对该上游多余）；
		// mirror=false 表示连镜像进 tool content 都看不到（更深的缺陷）。
		report["argsVisibleRaw"] = raw
		report["argsVisibleMirrored"] = mirror
		if rawDetail != "" {
			report["argsRawReply"] = rawDetail
		}
		if mirrorDetail != "" {
			report["argsMirroredReply"] = mirrorDetail
		}
	}

	// 3. usage 计数口径（阶梯尺寸）。
	small, big, err := probeUsageAccounting(client, base, key, model)
	if err != nil {
		report["usageProbeError"] = err.Error()
	} else {
		report["usagePromptSmallArgs"] = small
		report["usagePromptBigArgs"] = big
		report["usageTracksArgs"] = big > small+500
	}

	rawOut, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(st.Dir+"/probe-latest.json", rawOut, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "probe: write report: %v\n", err)
	}
	fmt.Println(string(rawOut))
	printProbeSummary(report)
	return nil
}

// pickProbeModel 选择探测模型：显式 slug > 该 provider 首个 listed 模型。
func pickProbeModel(reg *registry.Registry, provider *registry.Provider, args []string) string {
	if len(args) >= 2 {
		for _, m := range reg.Models {
			if m.Provider == provider.ID && (m.Slug == args[1] || m.UpstreamModel == args[1]) {
				return m.UpstreamModel
			}
		}
		return args[1] // 未注册名也允许探测（正是 [1m] 类路由验证所需）。
	}
	for _, m := range reg.Models {
		if m.Provider == provider.ID && m.Listed {
			return m.UpstreamModel
		}
	}
	for _, m := range reg.Models {
		if m.Provider == provider.ID {
			return m.UpstreamModel
		}
	}
	return ""
}

// probeBaseURL 与 server.(*Server).providerBaseURL 同规则（env 覆盖 >
// config base_url > 注册表默认），外加剥尾斜杠。
func probeBaseURL(st *state.State, p *registry.Provider) string {
	return strings.TrimSuffix(providerBaseURL(st, p), "/")
}

// probePost 发一个 chat completions 请求，返回解析后的响应。
func probePost(client *http.Client, base, key string, body map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("HTTP %d: non-JSON body %.200s", resp.StatusCode, payload)
	}
	if resp.StatusCode >= 400 {
		if e, ok := parsed["error"].(map[string]any); ok {
			if msg, ok := e["message"].(string); ok {
				return parsed, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
			}
		}
		return parsed, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, payload)
	}
	return parsed, nil
}

// probeModelsList 拉取 /models 并检查模型名是否在列。
func probeModelsList(client *http.Client, base, key, model string) (listed bool, exists bool, err error) {
	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, false, err
	}
	if resp.StatusCode >= 400 {
		return false, false, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, payload)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return true, false, nil // 列表端点返回非标准形态不算失败。
	}
	for _, m := range parsed.Data {
		if m.ID == model {
			return true, true, nil
		}
	}
	return true, false, nil
}

// probeArgumentsVisibility 用 16KB 埋密码的 arguments 测两形态可见性。
// raw 形态：密码只在 tool_calls[].function.arguments 里（上游丢字段则不可见）。
// mirror 形态：密码同时镜像在配对 tool content 头部（router 补偿路径）。
func probeArgumentsVisibility(client *http.Client, base, key, model string) (raw, mirror bool, rawDetail, mirrorDetail string, err error) {
	args := "echo 'SECRET_AT_ARGS_HEAD: " + probeNeedle + "' && echo " + strings.Repeat(probeFiller, 300)
	question := "What secret code appeared at the very beginning of the command you ran? Reply with the code only, nothing else."
	mkBody := func(mirrored bool) map[string]any {
		toolContent := "ran fine"
		if mirrored {
			toolContent = "Command (exec):\n" + args + "\nResult:\nran fine"
		}
		return map[string]any{
			"model": model, "max_tokens": 256, "stream": false,
			"messages": []any{
				map[string]any{"role": "user", "content": "Run this command then answer a question."},
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
					map[string]any{"id": "probe_1", "type": "function", "function": map[string]any{
						"name": "exec", "arguments": args}},
				}},
				map[string]any{"role": "tool", "tool_call_id": "probe_1", "content": toolContent},
				map[string]any{"role": "user", "content": question},
			},
		}
	}
	for _, mirrored := range []bool{false, true} {
		parsed, perr := probePost(client, base, key, mkBody(mirrored))
		if perr != nil {
			return raw, mirror, rawDetail, mirrorDetail, perr
		}
		reply := probeReplyText(parsed)
		hit := strings.Contains(reply, probeNeedle)
		if mirrored {
			mirror, mirrorDetail = hit, reply
		} else {
			raw, rawDetail = hit, reply
		}
	}
	return raw, mirror, rawDetail, mirrorDetail, nil
}

// probeUsageAccounting 用阶梯尺寸 arguments 测 prompt_tokens 是否随内容增长。
func probeUsageAccounting(client *http.Client, base, key, model string) (small, big int64, err error) {
	mk := func(repeat int) map[string]any {
		args := strings.Repeat(probeFiller, repeat)
		return map[string]any{
			"model": model, "max_tokens": 16, "stream": false,
			"messages": []any{
				map[string]any{"role": "user", "content": "go"},
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
					map[string]any{"id": "probe_2", "type": "function", "function": map[string]any{
						"name": "exec", "arguments": args}},
				}},
				map[string]any{"role": "tool", "tool_call_id": "probe_2", "content": "ok"},
				map[string]any{"role": "user", "content": "ok"},
			},
		}
	}
	for _, repeat := range []int{40, 500} { // ~2KB / ~32KB
		parsed, perr := probePost(client, base, key, mk(repeat))
		if perr != nil {
			return small, big, perr
		}
		usage, _ := parsed["usage"].(map[string]any)
		tokens, _ := usage["prompt_tokens"].(float64)
		if repeat == 40 {
			small = int64(tokens)
		} else {
			big = int64(tokens)
		}
	}
	return small, big, nil
}

// probeReplyText 提取首个 choice 的 content 文本。
func probeReplyText(parsed map[string]any) string {
	choices, _ := parsed["choices"].([]any)
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		message, _ := choice["message"].(map[string]any)
		if text, ok := message["content"].(string); ok {
			return text
		}
	}
	return ""
}

// printProbeSummary 打印人类可读结论（JSON 已完整输出，此处只给判定）。
func printProbeSummary(report map[string]any) {
	fmt.Fprintln(os.Stderr, "\n—— probe summary ——")
	if v, ok := report["modelExists"].(bool); ok && !v {
		fmt.Fprintf(os.Stderr, "⚠ 模型名 %v 不在 /models 列表（或列表不可用）——路由/命名可能已变化\n", report["model"])
	}
	if raw, ok := report["argsVisibleRaw"].(bool); ok {
		switch {
		case raw:
			fmt.Fprintln(os.Stderr, "✓ 上游正常回放 tool_call arguments（镜像补丁对该上游是冗余，可评估停用）")
		default:
			if mirror, ok2 := report["argsVisibleMirrored"].(bool); ok2 && mirror {
				fmt.Fprintln(os.Stderr, "△ 上游丢弃 tool_call arguments，但镜像进 tool content 可见 —— 保留 MirrorToolCallArguments")
			} else {
				fmt.Fprintln(os.Stderr, "✗ arguments 原始与镜像形态均不可见 —— 上游有更深的工具历史缺陷，需人工诊断")
			}
		}
	}
	if v, ok := report["usageTracksArgs"].(bool); ok {
		if !v {
			fmt.Fprintf(os.Stderr, "△ prompt_tokens 不随 arguments 尺寸增长（%v → %v）——计数口径异常，token 观测不可信\n",
				report["usagePromptSmallArgs"], report["usagePromptBigArgs"])
		} else {
			fmt.Fprintf(os.Stderr, "✓ prompt_tokens 随 arguments 增长（%v → %v）\n",
				report["usagePromptSmallArgs"], report["usagePromptBigArgs"])
		}
	}
}
