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

	// input → messages
	input, _ := responses["input"].([]any)
	// input 也允许是纯字符串（Responses API 的简写形态）。
	if text, ok := responses["input"].(string); ok {
		input = []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		}}
	}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		messages = append(messages, inputItemToMessages(item)...)
	}

	// tools：Responses 的扁平 function 形状 → chat 的嵌套 function 形状。
	// namespace 工具（协作运行时、app 工具集、MCP）chat 上游无法表达，
	// M1 与 LiteLLM 行为一致地丢弃，扁平 function（shell 等核心工具）保留。
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
			case "web_search", "web_search_preview":
				// chat-completions 上游没有对应物；丢弃。
			case "namespace", "mcp":
				// M3 移植完整的 namespace 拍平；M1 丢弃。
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
func inputItemToMessages(item map[string]any) []map[string]any {
	itemType, _ := item["type"].(string)
	switch itemType {
	case "":
		// 无 type 的 item 按 role 当作 message 处理（宽容历史形态）。
		if _, ok := item["role"]; ok {
			return messageItemToMessages(item)
		}
		return nil
	case "message":
		return messageItemToMessages(item)
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
		// 思维链历史对 chat 上游不可回放；M3 移植 carry 逻辑后
		// 会把 summary 并入后续 function_call 消息。M1 丢弃。
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
	default:
		// web_search_call、local_shell_call 等执行记录降级为
		// 文本占位，保留历史结构可读。
		return []map[string]any{{
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[%s item omitted from chat history]", orDefault(itemType, "unknown")),
			}},
		}}
	}
}

func messageItemToMessages(item map[string]any) []map[string]any {
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
		msg["content"] = translateContentParts(content)
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
// vision 路径）；其余降级为文本占位。
func translateContentParts(parts []any) []any {
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
			// 密文载荷 chat 上游无法消费，丢弃（subagent relay 在 M3
			// 负责在翻译前解出明文）。
		default:
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

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
