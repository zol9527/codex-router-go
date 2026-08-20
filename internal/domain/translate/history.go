package translate

import (
	"fmt"
)

// chat 历史修补，移植自 api-forwarder.mjs 的同名逻辑：
// 严格 chat-completions 上游（Go 订阅 / MiniMax / 类似）要求 tool
// result 消息紧跟携带匹配 tool_calls 的 assistant 消息。

// syntheticToolResult 是孤儿 tool_call 的补位结果：
// 短短一条机器可读的说明优于丢弃历史。
const syntheticToolResult = "[tool result unavailable: prior tool execution was interrupted or omitted from history]"

// MirrorToolCallArguments 把 assistant tool_calls 的命令参数镜像进配对
// tool 结果的头部，形态为 "Command (<name>):\n<arguments>\nResult:\n<output>"。
//
// 动机（2026-08-16 实锤）：Z.ai Coding Plan 的 glm-5.3 在组装模型输入时
// 丢弃 tool_calls[].function.arguments 的全部内容——既不进上下文也不进
// prompt_tokens 计数（模型自述 "no visibility into the command"），api.z.ai
// 与 open.bigmodel.cn 两端点行为一致。agent 的多轮行为依赖回看自己执行过
// 的命令（尤其 apply_patch 这类"载荷即工作产物"的调用），丢 arguments 后
// 模型只看到空回执，会无限重做已完成的工作（实测：同一文档任务重写 7 次）。
// 镜像进 tool 结果头部是上游透明的补偿：模型在"结果"旁边看到"命令"，
// 与 OpenAI 原生上游的可见性对齐。
//
// 镜像是 (name, arguments, output) 的纯函数——同一内容任何请求产出逐字节
// 相同的 tool 消息，前缀缓存不受影响。空参数（"{}"，Codex 的空探测调用）
// 只保留命令名行，不注入冗余的空参数。
func MirrorToolCallArguments(body map[string]any) {
	messages, ok := body["messages"].([]map[string]any)
	if !ok {
		return
	}
	for i, message := range messages {
		calls, _ := message["tool_calls"].([]any)
		if message["role"] != "assistant" || len(calls) == 0 {
			continue
		}
		// 以 call id 建索引：一条 assistant 可能聚合多轮 tool_call
		//（CoalesceAssistantMessages 的产物），配对的 tool 行紧随其后。
		byID := map[string]map[string]any{}
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			if id, _ := call["id"].(string); id != "" {
				byID[id] = fn
			}
		}
		for j := i + 1; j < len(messages) && messages[j]["role"] == "tool"; j++ {
			id, _ := messages[j]["tool_call_id"].(string)
			fn, ok := byID[id]
			if !ok {
				continue
			}
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			var header string
			if args != "" && args != "{}" {
				header = fmt.Sprintf("Command (%s):\n%s\nResult:\n", name, args)
			} else {
				header = fmt.Sprintf("Command (%s).\nResult:\n", name)
			}
			if content, ok := messages[j]["content"].(string); ok && content != "" {
				messages[j]["content"] = header + content
			} else {
				messages[j]["content"] = header
			}
		}
	}
}

// CoalesceAssistantMessages 合并连续 assistant 消息。
// Responses→chat 翻译会把"一段产出了 tool_calls 和文本的 assistant 回合"
// 展开成两条相邻 assistant 消息，tool result 从此不再紧跟 tool_calls。
// 连续 assistant 消息本就不是合法多轮形态，合并不改变良构对话。
func CoalesceAssistantMessages(messages []map[string]any) []map[string]any {
	if len(messages) < 2 {
		return messages
	}
	coalesced := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		previous := len(coalesced) - 1
		if previous >= 0 &&
			message["role"] == "assistant" && coalesced[previous]["role"] == "assistant" &&
			(hasToolCalls(coalesced[previous]) || hasToolCalls(message)) {
			combineAssistantContent(coalesced[previous], message)
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				existing, _ := coalesced[previous]["tool_calls"].([]any)
				coalesced[previous]["tool_calls"] = append(existing, calls...)
			}
			continue
		}
		coalesced = append(coalesced, message)
	}
	return coalesced
}

func hasToolCalls(message map[string]any) bool {
	calls, ok := message["tool_calls"].([]any)
	return ok && len(calls) > 0
}

func combineAssistantContent(target, source map[string]any) {
	sourceContent, ok := source["content"]
	if !ok || sourceContent == nil {
		return
	}
	targetContent, exists := target["content"]
	if !exists || targetContent == nil || targetContent == "" {
		target["content"] = sourceContent
		return
	}
	sourceText, sourceIsString := sourceContent.(string)
	targetText, targetIsString := targetContent.(string)
	if sourceIsString && targetIsString {
		target["content"] = targetText + "\n" + sourceText
		return
	}
	targetBlocks := anyToArray(targetContent)
	sourceBlocks := anyToArray(sourceContent)
	target["content"] = append(targetBlocks, sourceBlocks...)
}

func anyToArray(value any) []any {
	if arr, ok := value.([]any); ok {
		return arr
	}
	return []any{value}
}

// EnsureToolResultsForCalls 保证每个 assistant tool_calls 之后都跟着
// 完整匹配的 tool result：
//   - 孤儿 tool 行（不在 assistant 之后、id 不匹配、重复）被丢弃；
//   - 缺失的 call id 补合成结果。
func EnsureToolResultsForCalls(messages []map[string]any) []map[string]any {
	if len(messages) == 0 {
		return messages
	}
	repaired := make([]map[string]any, 0, len(messages))
	index := 0
	for index < len(messages) {
		message := messages[index]
		// tool 行只在其 assistant tool_calls 之后立即出现时才合法。
		if message["role"] == "tool" {
			index++
			continue
		}
		repaired = append(repaired, message)
		callIDs := toolCallIDs(message)
		if message["role"] != "assistant" || len(callIDs) == 0 {
			index++
			continue
		}
		index++
		// 收集紧随其后的 tool 行（按 call id 去重）。
		results := map[string]map[string]any{}
		for index < len(messages) && messages[index]["role"] == "tool" {
			toolMessage := messages[index]
			toolCallID, _ := toolMessage["tool_call_id"].(string)
			if toolCallID != "" && containsString(callIDs, toolCallID) {
				if _, seen := results[toolCallID]; !seen {
					results[toolCallID] = toolMessage
				}
			}
			index++
		}
		// 按 assistant 声明的顺序补齐：缺失的用合成结果占位。
		for _, callID := range callIDs {
			if toolMessage, ok := results[callID]; ok {
				repaired = append(repaired, toolMessage)
			} else {
				repaired = append(repaired, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      syntheticToolResult,
				})
			}
		}
	}
	return repaired
}

func toolCallIDs(message map[string]any) []string {
	calls, ok := message["tool_calls"].([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := call["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
