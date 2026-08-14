package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Tool result aging：旧的大体量工具结果在模型已行动之后仍然每回合重放。
// 只截"老的、文本的、超阈值"的结果；最新 FRONTIER 个结果逐字节保留。
// 移植自 tool-result-aging.mjs。

const (
	agingMinBytes = 32 * 1024
	agingFrontier = 4
	previewRunes  = 1024
)

var modelActedTypes = map[string]bool{
	"function_call": true, "custom_tool_call": true, "reasoning": true,
}

// AgingStats 报告本轮截断了多少。
type AgingStats struct {
	ToolResultsAged      int   `json:"toolResultsAged"`
	ToolResultBytesSaved int64 `json:"toolResultBytesSaved"`
}

// AgeToolResults 在 Responses input 数组上执行 aging。
// 返回（可能相同的）数组与统计。frontierCount 之外、且其后已有模型
// 行动（function_call/reasoning/assistant message）的文本结果被换成回执。
func AgeToolResults(input []any) ([]any, AgingStats) {
	var stats AgingStats
	// 从尾部数出 frontier：最新的 N 个 tool result 位置。
	frontierPositions := map[int]bool{}
	seen := 0
	for i := len(input) - 1; i >= 0 && seen < agingFrontier; i-- {
		if isToolOutputItem(input[i]) {
			frontierPositions[i] = true
			seen++
		}
	}
	// 每个 tool result 之后的"模型行动"决定它是否已旧。
	actedAfter := true // 尾部之后视作已行动（保守：最后的 result 也可能已旧）
	out := make([]any, len(input))
	for i := len(input) - 1; i >= 0; i-- {
		item, _ := input[i].(map[string]any)
		out[i] = input[i]
		if item == nil {
			continue
		}
		if actedAfter && isToolOutputItem(item) && !frontierPositions[i] {
			if text, ok := textualOutput(item); ok && len(text) >= agingMinBytes {
				receipt := resultReceipt(text, toolNameFor(input, item))
				stats.ToolResultsAged++
				stats.ToolResultBytesSaved += int64(len(text) - len(receipt))
				replaced := map[string]any{}
				for k, v := range item {
					replaced[k] = v
				}
				replaced["output"] = receipt
				out[i] = replaced
				actedAfter = false
				continue
			}
		}
		if isToolOutputItem(item) {
			actedAfter = false
			continue
		}
		if modelActed(item) {
			actedAfter = true
		}
	}
	return out, stats
}

func isToolOutputItem(item any) bool {
	m, _ := item.(map[string]any)
	if m == nil {
		return false
	}
	t, _ := m["type"].(string)
	return t == "function_call_output" || t == "custom_tool_call_output"
}

func modelActed(item map[string]any) bool {
	t, _ := item["type"].(string)
	if modelActedTypes[t] {
		return true
	}
	role, _ := item["role"].(string)
	return t == "message" && role == "assistant"
}

// textualOutput 只接受纯文本形态的结果；图像/混合内容原样保留。
func textualOutput(item map[string]any) (string, bool) {
	switch output := item["output"].(type) {
	case string:
		return output, true
	case []any:
		var parts []string
		for _, raw := range output {
			part, ok := raw.(map[string]any)
			if !ok {
				return "", false
			}
			t, _ := part["type"].(string)
			if t != "input_text" && t != "text" {
				return "", false
			}
			text, ok := part["text"].(string)
			if !ok {
				return "", false
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, ""), true
	default:
		return "", false
	}
}

// toolNameFor 从历史里找这个 result 对应的调用名（回执里的恢复指引用）。
func toolNameFor(input []any, outputItem map[string]any) string {
	callID, _ := outputItem["call_id"].(string)
	if callID == "" {
		return ""
	}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := item["type"].(string)
		if t != "function_call" && t != "custom_tool_call" {
			continue
		}
		if id, _ := item["call_id"].(string); id == callID {
			name, _ := item["name"].(string)
			return name
		}
	}
	return ""
}

func resultReceipt(value, toolName string) string {
	digest := sha256.Sum256([]byte(value))
	recovery := "Repeat the preceding tool call with the same arguments"
	if toolName != "" {
		recovery = fmt.Sprintf("Repeat the preceding %s call with the same arguments", toolName)
	}
	return strings.Join([]string{
		fmt.Sprintf("[Older tool result compacted by Codex Router after the model acted on it: %d bytes, sha256:%s.", len(value), hex.EncodeToString(digest[:])),
		recovery + " if exact or omitted content is needed. The original result remains in Codex; only this routed copy was compacted.]",
		"",
		"--- beginning of original result ---",
		safeHead(value),
		"--- omitted middle of original result ---",
		safeTail(value),
		"--- end of original result ---",
	}, "\n")
}

// safeHead/safeTail 按 rune 边界截预览。
func safeHead(value string) string {
	runes := []rune(value)
	if len(runes) <= previewRunes {
		return value
	}
	return string(runes[:previewRunes])
}

func safeTail(value string) string {
	runes := []rune(value)
	if len(runes) <= previewRunes {
		return value
	}
	return string(runes[len(runes)-previewRunes:])
}
