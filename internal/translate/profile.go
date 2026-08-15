package translate

import (
	"strings"

	"github.com/loyd/codex-router/internal/registry"
)

// glmEffortLadder 是 Codex effort 阶的全序（低→高）。
var glmEffortLadder = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

func ladderRank(effort string) int {
	for i, rung := range glmEffortLadder {
		if rung == effort {
			return i
		}
	}
	return -1
}

// glmEffort 把请求的 effort 钳制到模型自己声明的阶梯上。
// Z.ai 按"模型"而非按"厂商"声明档位：GLM-5.3 是 low/high/max，
// GLM-5.2 只有 high/max。Codex 顶档（xhigh/max/ultra）一律取模型最高档；
// 其余取"不高于请求档的最近已声明档"；低于模型下限则落在下限。
// 未声明阶梯（单档模型）返回 ""，表示该模型根本不支持此参数。
func glmEffort(requested string, levels []string) string {
	var declared []string
	for _, level := range levels {
		if ladderRank(level) >= 0 {
			declared = append(declared, level)
		}
	}
	if len(declared) == 0 {
		return ""
	}
	switch requested {
	case "xhigh", "max", "ultra":
		return declared[len(declared)-1]
	}
	ceiling := ladderRank("high")
	if rank := ladderRank(requested); rank >= 0 {
		ceiling = rank
	}
	best := declared[0]
	for _, level := range declared {
		if ladderRank(level) <= ceiling {
			best = level
		}
	}
	return best
}

// ApplyRequestProfile 把模型的 requestProfile 施加到已翻译的 chat 请求体。
// 这是 api-forwarder.mjs 中对应分支的移植，只保留本 fork 存活的 provider
// 所需的 profile：glm-thinking（zai-coding 全系）与默认（opencode-go）。
func ApplyRequestProfile(body map[string]any, requestedEffort string, model *registry.Model) {
	if body == nil || model == nil {
		return
	}
	switch model.RequestProfile {
	case "glm-thinking":
		// GLM 套餐的 thinking 必开；effort 钳到模型声明档位；
		// 采样参数会被 Z.ai 拒绝（要求 temperature=1.0）。
		body["thinking"] = map[string]any{"type": "enabled"}
		levels := make([]string, 0, len(model.ReasoningLevels))
		for _, level := range model.ReasoningLevels {
			levels = append(levels, level.Effort)
		}
		if len(levels) > 1 {
			if effort := glmEffort(requestedEffort, levels); effort != "" {
				body["reasoning_effort"] = effort
			} else {
				delete(body, "reasoning_effort")
			}
		} else {
			delete(body, "reasoning_effort")
		}
		delete(body, "temperature")
		delete(body, "top_p")
	case "deepseek-thinking":
		// opencode 的 DeepSeek thinking 模式要求历史 assistant 回合
		// 把 reasoning_content 传回 —— fork 出来的协作子代理首轮就带
		// 父线程历史，翻译若丢弃思维链即 400（实发事故 2026-08-15：
		// deepseek-v4-flash 子代理）。翻译期已把 reasoning item 的文本
		// 挂在 _reasoning_carry 标记上，此处提升为正式字段。
		// 与 Node 版直连 DeepSeek 分支有意不同：不再发送 thinking 对象
		// 或清洗采样参数 —— opencode 中继的 thinking 默认已开（输出侧
		// reasoning_content 增量实测回流），未知字段反而可能被中继拒绝。
		promoteReasoningCarry(body)
		// thinking 模式拒绝强制 tool_choice（兼容性探测发 "required"、
		// 协作载荷中继发 function 对象），降级 auto 保住工具调用。
		if choice, ok := body["tool_choice"]; ok && choice != "none" {
			body["tool_choice"] = "auto"
		}
	default:
		// 无特殊 profile：reasoning.effort 已收集为 requestedEffort，
		// 作为标准 reasoning_effort 透传（上游不认时会拒绝或忽略，
		// 这与 LiteLLM 时代的行为一致）。
		if requestedEffort != "" {
			body["reasoning_effort"] = requestedEffort
		}
	}
	// thinking 载荷里可能残留 Responses 侧的 reasoning 对象形态，清掉。
	delete(body, "reasoning")
	delete(body, "web_search_options")
	// 内部携带标记绝不外发：未提升（deepseek-thinking）的一律剥除。
	if model.RequestProfile != "deepseek-thinking" {
		promoteReasoningCarryBody(body, false)
	}
}

// promoteReasoningCarry 把消息里的 _reasoning_carry 标记提升为
// reasoning_content 并剥除标记。
func promoteReasoningCarry(body map[string]any) {
	promoteReasoningCarryBody(body, true)
}

func promoteReasoningCarryBody(body map[string]any, promote bool) {
	messages, ok := body["messages"].([]map[string]any)
	if !ok {
		return
	}
	for _, message := range messages {
		carry, _ := message[reasoningCarryKey].(string)
		if carry == "" {
			continue
		}
		delete(message, reasoningCarryKey)
		if promote {
			message["reasoning_content"] = carry
		}
	}
}

// UpstreamHeadersFrom 构造发往 chat 上游的请求头：只保留转发白名单外的
// 常规头，剥掉调用方/网关的身份标记（authorization、x-codex-*、
// chatgpt-account-id、originator、user-agent、accept-encoding），
// 换成 provider 凭据与固定 UA。
func UpstreamHeadersFrom(inbound map[string][]string, apiKey string, version string) map[string]string {
	headers := map[string]string{}
	for name, values := range inbound {
		lower := strings.ToLower(name)
		if isHopByHop(lower) || lower == "authorization" || lower == "x-api-key" {
			continue
		}
		if strings.HasPrefix(lower, "x-msh-") || strings.HasPrefix(lower, "x-codex-") ||
			strings.HasPrefix(lower, "x-openai-") || lower == "chatgpt-account-id" {
			continue
		}
		if lower == "originator" || lower == "user-agent" || lower == "accept-encoding" ||
			lower == "host" || lower == "content-length" || lower == "content-type" {
			continue
		}
		if len(values) > 0 {
			headers[name] = strings.Join(values, ", ")
		}
	}
	headers["Authorization"] = "Bearer " + apiKey
	headers["User-Agent"] = "codex-router/" + version
	headers["Accept-Encoding"] = "identity"
	return headers
}

var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true,
}

func isHopByHop(lower string) bool { return hopByHop[lower] }
