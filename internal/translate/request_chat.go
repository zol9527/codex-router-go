// Package translate 实现 Codex 说的 Responses 协议与 chat-completions
// 上游（zai-coding / opencode-go）之间的双向翻译。请求方向把
// input[] / tools / reasoning 翻成 messages / tools / reasoning_effort；
// 响应方向把上游的 SSE delta 流重组成 Responses 事件序列。
//
// JSON 全部走 map[string]any 动态形状：协议字段在上游间差异大，
// 保真透传未知字段比静态结构体更接近正确行为。
package translate

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// CompactionPrefix 与 Node 版一致：v2 压缩摘要的 kcr1: base64 封装。
const CompactionPrefix = "kcr1:"

// reasoningCarryKey 是翻译期携带思维链的内部标记字段：reasoning item
// 的文本挂到其后首条 assistant 消息上，ApplyRequestProfile 按
// requestProfile 决定提升为 reasoning_content（deepseek-thinking）或
// 剥除（其余上游）。字段去留必须按上游契约门控 —— opencode 的
// DeepSeek thinking 模式要求回传 reasoning_content（fork 出来的协作
// 线程首轮就带父历史，丢弃即 400），而官方 DeepSeek API 对输入里的
// reasoning_content 反而报 400，规则相反。
const reasoningCarryKey = "_reasoning_carry"

const summaryPrefix = "Another language model started this task and produced a continuation summary. Use it to continue without repeating completed work:"

// ChatRequest 是翻译产物：发给 chat-completions 上游的请求体与
// 翻译过程中收集的上下文（当前 effort、模型 slug）。
type ChatRequest struct {
	Body map[string]any
	// RequestedEffort 是 Codex 在 Responses 请求里声明的 reasoning.effort，
	// 供 requestProfile 阶梯钳制使用。
	RequestedEffort string
	// HasStream 记录调用方是否要求流式。
	HasStream bool
	// CustomTools 是请求里以 custom 工具形态声明的工具名（code-mode
	// exec 等）：chat 上游只见 function，声明被翻译成 {input: string}
	// 单参形态；响应侧据此把对这些名字的调用还原成 custom_tool_call
	// item —— 否则 Codex 的 custom 规格工具收到 function 形态调用会报
	// "invoked with incompatible payload"（2026-08-15 deepseek 子代理
	// 实发事故：模型自造 {"cmd"/"command"} 载荷三连击穿）。
	CustomTools []string
	// OmittedItemTypes / OmittedPartTypes 记录翻译中因类型未知而被
	// 降级（item 占位符替换 / part 丢弃）的形状名。非空说明客户端
	// 协议出现了翻译器不认识的类型，内容可能静默丢失——server 层
	// 据此打告警日志（2026-08-18 agent_message 任务书被占位符吞掉的
	// 事故全程 HTTP 200、零错误线索，观测位是为下一次事故准备的）。
	OmittedItemTypes []string
	OmittedPartTypes []string
}

// TranslateToChat 把一个 Responses 请求体翻译成 chat-completions 请求。
// registry 层已完成 model 字段的替换；这里只做协议形状翻译。
func TranslateToChat(responses map[string]any) (*ChatRequest, error) {
	out := map[string]any{}
	chat := &ChatRequest{Body: out}

	// 直通字段：chat-completions 语义相同或兼容的参数原样保留。
	for _, key := range []string{"model", "temperature", "top_p", "stream",
		"stop", "max_tokens", "max_completion_tokens", "presence_penalty",
		"frequency_penalty", "seed", "user", "n", "logprobs", "top_logprobs",
		"parallel_tool_calls", "service_tier"} {
		if v, ok := responses[key]; ok {
			out[key] = v
		}
	}
	if s, ok := out["stream"].(bool); ok {
		chat.HasStream = s
	}

	// Codex 专属标记，严格上游会拒绝未知字段。
	delete(out, "client_metadata")
	// Responses 专属字段在 chat-completions 上没有对应物。
	delete(out, "store")
	delete(out, "include")
	delete(out, "previous_response_id")
	delete(out, "prompt_cache_key")
	delete(out, "truncation")
	delete(out, "metadata")

	// instructions → 首条 system 消息。
	var messages []map[string]any
	if instructions, ok := responses["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	// reasoning.effort → reasoning_effort（具体取值由 requestProfile 钳制）。
	if reasoning, ok := responses["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			chat.RequestedEffort = effort
		}
	}

	// input → messages。reasoning item 不直接产出消息：文本进
	// pendingReasoning，等紧随其后的 assistant 输出（文本消息或
	// tool_calls）落地后挂到它头上（见 reasoningCarryKey 注释）。
	input, _ := responses["input"].([]any)
	// input 也允许是纯字符串（Responses API 的简写形态）。
	if text, ok := responses["input"].(string); ok {
		input = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		}}
	}
	pendingReasoning := ""
	omissions := &omissionLog{}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if itemType, _ := item["type"].(string); itemType == "reasoning" {
			if text := reasoningItemText(item); text != "" {
				if pendingReasoning != "" {
					pendingReasoning += "\n"
				}
				pendingReasoning += text
			}
			continue
		}
		expanded := inputItemToMessages(item, omissions)
		if pendingReasoning != "" {
			consumed := false
			for _, message := range expanded {
				if message["role"] == "assistant" && !consumed {
					message[reasoningCarryKey] = pendingReasoning
					consumed = true
				}
			}
			if consumed {
				pendingReasoning = ""
			} else if role, _ := item["role"].(string); role != "assistant" {
				// user/tool/system 边界跨不过去；被丢弃的空 assistant
				// 消息（宣告 tool call 的 filler）不消耗 pending，
				// 让它落到后面的 function_call 上。
				pendingReasoning = ""
			}
		}
		messages = append(messages, expanded...)
	}

	// tools：Responses 的扁平 function 形状 → chat 的嵌套 function 形状。
	// namespace / mcp 形态的协作运行时、app 工具集、MCP server 已在
	// server 管线（routed.go 的 FlattenNamespaceTools）拍平成普通
	// function 到达这里；此处仍见 namespace/mcp 只可能是绕过管线的
	// 直连调用，兜底丢弃。扁平 function（shell 等核心工具）保留。
	if tools, ok := responses["tools"].([]any); ok {
		var chatTools []any
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch tool["type"] {
			case "function":
				fn := map[string]any{"name": tool["name"]}
				for _, key := range []string{"description", "parameters", "strict"} {
					if v, ok := tool[key]; ok {
						fn[key] = v
					}
				}
				chatTools = append(chatTools, map[string]any{
					"type": "function", "function": fn,
				})
			case "custom":
				// Responses 的 custom 工具（自由文本载荷，code-mode exec
				// 就是这个形态）在 chat 上游没有对应物：伪装成单参
				// function（input 字符串），名字记进 CustomTools 供响应
				// 侧还原。静默丢弃会让模型从历史里模仿自造载荷形状。
				name, _ := tool["name"].(string)
				if name == "" {
					break
				}
				description, _ := tool["description"].(string)
				chatTools = append(chatTools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        name,
						"description": description + "\n\nPass the entire freeform payload for this tool as the `input` string parameter.",
						"parameters": map[string]any{
							"type":                 "object",
							"properties":           map[string]any{"input": map[string]any{"type": "string", "description": "The freeform text payload for this tool"}},
							"required":             []any{"input"},
							"additionalProperties": false,
						},
					},
				})
				chat.CustomTools = append(chat.CustomTools, name)
			case "web_search", "web_search_preview":
				// chat-completions 上游没有对应物；丢弃。
			case "namespace", "mcp":
				// 正常流量在 server 管线已拍平为普通 function，
				// 这里只兜未过管线的直连调用。
			}
		}
		if len(chatTools) > 0 {
			out["tools"] = chatTools
		}
	}

	// tool_choice：枚举值直通；对象形态从扁平改成嵌套。
	if tc, ok := responses["tool_choice"]; ok {
		out["tool_choice"] = translateToolChoice(tc)
	}

	messages = CoalesceAssistantMessages(messages)
	messages = EnsureToolResultsForCalls(messages)
	if len(messages) > 0 {
		out["messages"] = messages
	}
	chat.OmittedItemTypes = omissions.items
	chat.OmittedPartTypes = omissions.parts
	return chat, nil
}

func translateToolChoice(tc any) any {
	switch value := tc.(type) {
	case string:
		return value
	case map[string]any:
		if value["type"] == "function" {
			if name, ok := value["name"].(string); ok {
				return map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		}
		if fn, ok := value["function"].(map[string]any); ok {
			return map[string]any{"type": "function", "function": fn}
		}
		return "auto"
	default:
		return "auto"
	}
}

// inputItemToMessages 把一个 Responses input item 展开成零或多个
// chat messages。function_call 展开成带 tool_calls 的 assistant 消息，
// 后续的 CoalesceAssistantMessages 会把它与前一条 assistant 合并。
// omissions 记录降级为占位符的未知类型，供调用方告警。
func inputItemToMessages(item map[string]any, omissions *omissionLog) []map[string]any {
	itemType, _ := item["type"].(string)
	switch itemType {
	case "":
		// 无 type 的 item 按 role 当作 message 处理（宽容历史形态）。
		if _, ok := item["role"]; ok {
			return messageItemToMessages(item, omissions)
		}
		return nil
	case "message":
		return messageItemToMessages(item, omissions)
	case "function_call":
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		return []map[string]any{{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": orDefault(args, "{}"),
				},
			}},
		}}
	case "custom_tool_call":
		name, _ := item["name"].(string)
		inputText, _ := item["input"].(string)
		callID, _ := item["call_id"].(string)
		return []map[string]any{{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": fmt.Sprintf("%q", inputText),
				},
			}},
		}}
	case "function_call_output", "custom_tool_call_output":
		callID, _ := item["call_id"].(string)
		return []map[string]any{{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      toolOutputToText(item["output"]),
		}}
	case "reasoning":
		// 思维链历史在 TranslateToChat 的输入循环里被截住并携带
		// （reasoningCarryKey），不会走到这里；保留分支兜底。
		return nil
	case "compaction_trigger":
		// v2 压缩触发标记不进对话历史。
		return nil
	case "compaction":
		summary := decodeSummaryText(item["encrypted_content"])
		if summary == "" {
			summary = "[Earlier conversation history was compacted in an unreadable format.]"
		}
		return []map[string]any{{
			"role": "user",
			"content": []any{map[string]any{
				"type": "text", "text": summaryPrefix + "\n\n" + summary,
			}},
		}}
	case "agent_message":
		return agentMessageItemToMessages(item, omissions)
	default:
		// web_search_call、local_shell_call 等执行记录降级为
		// 文本占位，保留历史结构可读。内容承载型 item 落到这里
		// 就是静默丢内容（agent_message 事故），必须留名给上层告警。
		omissions.item(itemType)
		return []map[string]any{{
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[%s item omitted from chat history]", orDefault(itemType, "unknown")),
			}},
		}}
	}
}

// omissionLog 收集翻译期因类型未知而被降级的形状名（去重）。
// translate 层保持无 IO 依赖，只负责收集；日志由 server 层落地。
type omissionLog struct {
	items []string
	parts []string
}

func (o *omissionLog) item(kind string) {
	o.items = appendUnique(o.items, kind)
}

func (o *omissionLog) part(kind string) {
	o.parts = appendUnique(o.parts, kind)
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// agentMessageItemToMessages 把 Codex 多代理协作的跨代理消息
// （spawn/followup 的 NEW_TASK 任务书、send_message 与 RESULT 回传）
// 映射为 user 消息。content 的 input_text part 是投递信封，
// encrypted_content part 承载任务正文——Codex 自产自销的透传载体，
// 实测为明文（2026-08-18 rollout 取证），无需解密即可收编。
//
// 绝不能落入 default 的占位符分支：子代理的整个任务书只有这一个
// item，占位符会让 explorer 在任务盲状态下空转或臆测任务
// （2026-08-18 实发：两个 explorer 分别"请求重发任务"和
// 臆测出 onboardingv2 乱搜两分钟）。历史回放时正文也可能直接
// 放 message/text 字符串字段，做兜底；全部无可读文本则丢弃，
// 与空消息策略一致。
func agentMessageItemToMessages(item map[string]any, omissions *omissionLog) []map[string]any {
	var texts []string
	if parts, ok := item["content"].([]any); ok {
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			var text string
			switch part["type"] {
			case "input_text", "output_text", "text":
				text, _ = part["text"].(string)
			case "encrypted_content":
				text, _ = part["encrypted_content"].(string)
			default:
				if partType, _ := part["type"].(string); partType != "" {
					omissions.part(partType)
				}
			}
			if strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	if len(texts) == 0 {
		for _, key := range []string{"message", "text"} {
			if text, _ := item[key].(string); strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return []map[string]any{{
		"role":    "user",
		"content": strings.Join(texts, "\n"),
	}}
}

func messageItemToMessages(item map[string]any, omissions *omissionLog) []map[string]any {
	role, _ := item["role"].(string)
	switch role {
	case "user", "system", "developer":
		if role == "developer" {
			role = "system"
		}
	default:
		role = "assistant"
	}
	msg := map[string]any{"role": role}

	// content：字符串直通；parts 数组按类型翻译。
	switch content := item["content"].(type) {
	case string:
		msg["content"] = content
	case []any:
		msg["content"] = translateContentParts(content, omissions)
	default:
		msg["content"] = ""
	}
	// Responses 历史 item 可能自带 tool_calls（Codex 的 assistant 消息形态）。
	if toolCalls, ok := item["tool_calls"].([]any); ok && len(toolCalls) > 0 {
		msg["tool_calls"] = normalizeHistoryToolCalls(toolCalls)
		if msg["content"] == "" {
			msg["content"] = nil
		}
		return []map[string]any{msg}
	}
	// 空 content 的消息整个丢弃：上游拒绝空文本 part，
	// Codex 会在 tool call 周围发这种 filler assistant 消息。
	if parts, ok := msg["content"].([]any); ok && len(parts) == 0 {
		return nil
	}
	// name 字段（具名参与者）在 chat 上无对应，丢弃。
	return []map[string]any{msg}
}

// translateContentParts 把 Responses content parts 翻成 chat parts。
// input_text/output_text/text → text；input_image → image_url（留给
// vision 路径）；未知类型降级为文本占位并记入 omissions。
func translateContentParts(parts []any, omissions *omissionLog) []any {
	out := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			if text, ok := raw.(string); ok {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
			continue
		}
		switch part["type"] {
		case "input_text", "output_text", "text", "summary_text", "refusal":
			kind := "text"
			text, _ := part["text"].(string)
			if part["type"] == "refusal" {
				if r, ok := part["refusal"].(string); ok {
					text = r
				}
			}
			// 空 text part 直接跳过：上游拒绝空文本 part。
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, map[string]any{"type": kind, "text": text})
		case "input_image":
			imageURL, _ := part["image_url"].(string)
			if detail, ok := part["detail"]; ok {
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURL, "detail": detail},
				})
			} else {
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": imageURL},
				})
			}
		case "encrypted_content":
			// message item 的密文 part 丢弃：server 层 agentrelay
			// 已在翻译前把协作载荷换成明文（agent_message 的明文
			// 收编在 agentMessageItemToMessages），这里只剩 reasoning
			// 回放类的真密文，chat 上游无法消费。
		default:
			if partType, _ := part["type"].(string); partType != "" {
				omissions.part(partType)
			}
			if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		}
	}
	return out
}

// normalizeHistoryToolCalls 把历史里的 tool_calls 归一成 chat 形状。
func normalizeHistoryToolCalls(toolCalls []any) []any {
	out := make([]any, 0, len(toolCalls))
	for _, raw := range toolCalls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := call["id"].(string)
		fn := map[string]any{}
		if nested, ok := call["function"].(map[string]any); ok {
			if name, ok := nested["name"].(string); ok {
				fn["name"] = name
			}
			if args, ok := nested["arguments"].(string); ok {
				fn["arguments"] = orDefault(args, "{}")
			}
		}
		out = append(out, map[string]any{"id": id, "type": "function", "function": fn})
	}
	return out
}

// toolOutputToText 展开 function_call_output 的 output 字段。
// 形态是 JSON 数组字符串 [{"type":"output_text","text":"..."}]，
// 也可能是纯字符串；两者都要变成 chat tool 消息的纯文本。
func toolOutputToText(output any) string {
	switch value := output.(type) {
	case string:
		if parts, ok := parseOutputParts(value); ok {
			return parts
		}
		return value
	case []any:
		return contentPartsToText(value)
	case map[string]any:
		return contentPartsToText([]any{value})
	default:
		return fmt.Sprintf("%v", value)
	}
}

func parseOutputParts(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "[") {
		return "", false
	}
	var parts []any
	if err := json.Unmarshal([]byte(trimmed), &parts); err != nil {
		return "", false
	}
	return contentPartsToText(parts), true
}

func contentPartsToText(parts []any) string {
	var buf bytes.Buffer
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok {
			buf.WriteString(text)
			continue
		}
		if output, ok := part["output"].(string); ok {
			buf.WriteString(output)
		}
	}
	return buf.String()
}

// decodeSummaryText 解出 kcr1: base64 压缩摘要。
func decodeSummaryText(value any) string {
	encoded, ok := value.(string)
	if !ok || !strings.HasPrefix(encoded, CompactionPrefix) {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encoded, CompactionPrefix))
	if err != nil {
		return ""
	}
	return string(decoded)
}

// reasoningItemText 提取 reasoning item 的可读文本（移植 Node 版
// reasoningItemText）：summary 字符串/数组优先（Codex 回放的形态），
// content 兜底 —— 部分 thinking 上游把思维链放 content 数组而不是
// summary。无可读文本返回 ""。
func reasoningItemText(item map[string]any) string {
	if text := textFromPartsField(item["summary"]); text != "" {
		return text
	}
	return textFromPartsField(item["content"])
}

// textFromPartsField 兼容字符串与 [{text:...}] 数组两种字段形态。
func textFromPartsField(value any) string {
	switch field := value.(type) {
	case string:
		return field
	case []any:
		var texts []string
		for _, raw := range field {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					texts = append(texts, text)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
