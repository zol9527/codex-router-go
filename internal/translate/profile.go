package translate

import (
	"strings"

	"github.com/loyd/codex-router/internal/registry"
)

// EffortLadder 是 effort 档的全序（低→高）——router 的规范阶梯，
// 所有档位归一（钳制/区间展开）都在这一条序上做。
var EffortLadder = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

// CodexEffortRungs 是 Codex 桌面端会请求的档位词汇（enabled-reasoning-
// efforts 不会超出这个集合）。catalog 发布集按它做区间展开，保证
// Codex 侧校验（spawn 的 reasoning_effort 必须 ∈ supported 集合，
// 校验发生在请求到达 router 之前）永远不会因词汇差异拒掉请求。
var CodexEffortRungs = []string{"low", "medium", "high", "xhigh"}

func ladderRank(effort string) int {
	for i, rung := range EffortLadder {
		if rung == effort {
			return i
		}
	}
	return -1
}

func absRank(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// ClampEffort 把请求的 effort 归到模型声明的档位阶梯上（区间映射）：
// 每个声明档位"拥有"请求阶梯上离它最近的一段连续区间。
// 规则：
//   - 顶档请求（xhigh/max/ultra）语义就是"拉满"，一律取模型最高声明档；
//   - 其余请求取绝对距离最近的声明档，平手取较低者（严格小于保证
//     升序遍历时先到者胜，落在省钱侧）；
//   - 未声明阶梯（levels 全部不在规范阶梯上）或不认识的请求档返回 ""，
//     调用方据此决定透传还是删参。
//
// 例：deepseek 声明 [minimal, high]（距 3 级），请求 medium（距 high 1 级、
// 距 minimal 2 级）→ high；请求 low → minimal。GLM-5.3 声明
// [low, high, max]，请求 medium 平手 → low（与旧"就近向下"行为一致）。
func ClampEffort(requested string, levels []string) string {
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
	rank := ladderRank(requested)
	if rank < 0 {
		return ""
	}
	best, bestDist := declared[0], absRank(ladderRank(declared[0]), rank)
	for _, level := range declared[1:] {
		if d := absRank(ladderRank(level), rank); d < bestDist {
			best, bestDist = level, d
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
			// 区间钳制（ClampEffort）；单档模型 zai 拒收该参数，删除。
			if effort := ClampEffort(requestedEffort, levels); effort != "" {
				body["reasoning_effort"] = effort
			} else {
				delete(body, "reasoning_effort")
			}
		} else {
			delete(body, "reasoning_effort")
		}
		delete(body, "temperature")
		delete(body, "top_p")
		// Z.ai 服务端丢弃 tool_calls 的 arguments（见 MirrorToolCallArguments
		// 注释），glm-thinking 的 provider 全系中招——镜像进 tool 结果头部
		// 恢复模型可见性。仅此 profile 启用：参数处理正常的上游（如
		// opencode-go）里 arguments 本就可见，镜像属于纯冗余。
		MirrorToolCallArguments(body)
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
		// 无特殊 profile：effort 先按模型声明档位做区间钳制 ——
		// 自定义模型常见的 400 根源是 Codex 词汇表的档位上游不认
		// （2026-08-20 litellm/volcengine 实发）。模型未声明档位时
		// 原样透传（与 LiteLLM 时代的行为一致，上游不认时自行拒绝
		// 或忽略）。
		if requestedEffort != "" {
			levels := make([]string, 0, len(model.ReasoningLevels))
			for _, level := range model.ReasoningLevels {
				levels = append(levels, level.Effort)
			}
			if effort := ClampEffort(requestedEffort, levels); effort != "" {
				body["reasoning_effort"] = effort
			} else {
				body["reasoning_effort"] = requestedEffort
			}
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
