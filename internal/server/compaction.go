package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/httpx"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/translate"
	"github.com/loyd/codex-router/internal/usage"
)

// routed compaction，移植自 router.mjs 的 compaction 族函数。
//
// 压缩是把整段对话重放给同一个 provider 生成交接摘要：
//   - v1（POST /responses/compact）：返回 {output: [尾预算内的 user 消息
//     + 摘要消息]}，Codex 拿去续会话；
//   - v2（input 尾部 compaction_trigger）：返回 kcr1: base64 摘要的
//     compaction item（JSON 或合成 SSE，按请求的 stream 字段）。

const compactPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another language model that will resume the task.

Include current progress, key decisions, constraints, user preferences, remaining steps, and critical data or references. Be concise, structured, and focused on seamless continuation.`

const summaryPrefixText = "Another language model started this task and produced a continuation summary. Use it to continue without repeating completed work:"

// compactBudget 是 v1 输出携带的原始 user 消息字节预算。
const compactBudget = 80_000

// isCompactionV2 判断 v2 压缩触发（input 末尾是 compaction_trigger）。
func isCompactionV2(payload map[string]any) bool {
	input, ok := payload["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}
	last, ok := input[len(input)-1].(map[string]any)
	return ok && last["type"] == "compaction_trigger"
}

// encodeSummaryText 把摘要封装成 kcr1: base64（与请求方向的解码配对）。
func encodeSummaryText(summary string) string {
	return translate.CompactionPrefix + base64.StdEncoding.EncodeToString([]byte(summary))
}

// handleRoutedCompaction 执行压缩并写出结果。
func (s *Server) handleRoutedCompaction(w http.ResponseWriter, r *http.Request,
	payload map[string]any, model *registry.Model, provider *registry.Provider,
	credential string, v2 bool, route string, started time.Time) {

	providerID := s.opt.Registry.CanonicalProviderID(provider.ID)

	// 请求构造：整段对话 + 压缩指令，非流式、无工具。
	// 压缩重放协作条目，agent 载荷解析与普通回合相同（缓存按密文键，
	// 已解析过的会话零额外成本）；aging 亦与普通回合一致。
	aging := translate.AgingStats{}
	compactionPayload := map[string]any{}
	for k, v := range payload {
		compactionPayload[k] = v
	}
	if input, ok := compactionPayload["input"].([]any); ok {
		compactionPayload["input"] = s.normalizeRoutedAgentInput(r.Context(), input)
	}
	if input, ok := compactionPayload["input"].([]any); ok {
		if aged, stats := translate.AgeToolResults(input); true {
			_ = aged
			aging = stats
			// v1 压缩保留链式结构、v2 过滤触发标记后重放。
			filtered := make([]any, 0, len(input))
			for _, raw := range input {
				if item, ok := raw.(map[string]any); ok && item["type"] == "compaction_trigger" {
					continue
				}
				filtered = append(filtered, raw)
			}
			compactionPayload["input"] = append(filtered, userMessageItem(compactPrompt))
		}
	} else {
		compactionPayload["input"] = []any{userMessageItem(compactPrompt)}
	}
	compactionPayload["model"] = model.UpstreamModel
	compactionPayload["stream"] = false
	// 空工具列表已禁用工具调用；tool_choice:"none" 与之配套反被
	// 部分上游拒绝，故整个字段省略。
	compactionPayload["tools"] = []any{}
	delete(compactionPayload, "previous_response_id")
	delete(compactionPayload, "client_metadata")

	chat, err := translate.TranslateToChat(compactionPayload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", err.Error()))
		return
	}
	translate.ApplyRequestProfile(chat.Body, chat.RequestedEffort, model)
	chat.Body["model"] = model.UpstreamModel
	normalized, err := json.Marshal(chat.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Unable to encode request."))
		return
	}

	headers := translate.UpstreamHeadersFrom(headerMap(r.Header), credential, Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "application/json"
	target := strings.TrimSuffix(providerBaseURL(provider), "/") + "/chat/completions"

	resp, _, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, target, headers, normalized, httpx.DefaultRetryOptions())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
		s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 502,
			DurationMs:      time.Since(started).Milliseconds(),
			ToolResultsAged: aging.ToolResultsAged, ToolResultBytesSaved: aging.ToolResultBytesSaved})
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The compaction response could not be read."))
		return
	}
	if len(raw) >= 32<<20 {
		writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error", "Compact response is too large."))
		return
	}
	var chatBody map[string]any
	if err := json.Unmarshal(raw, &chatBody); err != nil {
		writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The upstream compaction response was not valid JSON."))
		return
	}
	if resp.StatusCode >= 400 {
		retryAfter := 0
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			fmt.Sscanf(ra, "%d", &retryAfter)
		}
		s.writeUpstreamError(w, provider, model, &upstreamFailure{
			status: resp.StatusCode, bodyText: string(raw), retryAfter: retryAfter,
		})
		s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: resp.StatusCode,
			DurationMs: time.Since(started).Milliseconds()})
		return
	}

	summary := extractChatResponseText(chatBody)
	usageTokens := chatUsageTokens(chatBody)

	if !v2 {
		// v1：尾预算内的 user 消息 + 摘要。
		output := compactOutput(payload["input"], summary)
		writeJSON(w, http.StatusOK, map[string]any{"output": output})
		s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 200,
			DurationMs:  time.Since(started).Milliseconds(),
			InputTokens: usageTokens.prompt, OutputTokens: usageTokens.completion,
			ToolResultsAged: aging.ToolResultsAged, ToolResultBytesSaved: aging.ToolResultBytesSaved})
		return
	}

	// v2：kcr1: base64 摘要的 compaction item。
	item := map[string]any{
		"type":              "compaction",
		"id":                "cmp_" + randomHexID(),
		"encrypted_content": encodeSummaryText(summary),
	}
	if stream, _ := payload["stream"].(bool); !stream {
		writeJSON(w, http.StatusOK, compactionSnapshot(payload["model"], item, "completed"))
	} else {
		writeCompactionSSE(w, payload["model"], item)
	}
	s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 200,
		DurationMs:  time.Since(started).Milliseconds(),
		InputTokens: usageTokens.prompt, OutputTokens: usageTokens.completion,
		ToolResultsAged: aging.ToolResultsAged, ToolResultBytesSaved: aging.ToolResultBytesSaved})
}

// userMessageItem 构造一个 user 文本消息 item。
func userMessageItem(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

// extractUserMessages 提取 input 里的 user 文本消息（v1 尾预算用）。
func extractUserMessages(input any) []string {
	list, ok := input.([]any)
	if !ok {
		return nil
	}
	var messages []string
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, has := item["type"]; has && t != "message" {
			continue
		}
		if item["role"] != "user" {
			continue
		}
		text := ""
		switch content := item["content"].(type) {
		case string:
			text = content
		case []any:
			var parts []string
			for _, partRaw := range content {
				part, ok := partRaw.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := part["type"].(string); t == "input_text" || t == "text" {
					if s, ok := part["text"].(string); ok {
						parts = append(parts, s)
					}
				}
			}
			text = strings.Join(parts, "")
		}
		if strings.TrimSpace(text) != "" {
			messages = append(messages, text)
		}
	}
	return messages
}

// compactOutput 选择尾预算内的 user 消息（从最新往回），加摘要消息。
func compactOutput(input any, summary string) []any {
	remaining := compactBudget
	var selected []string
	messages := extractUserMessages(input)
	for i := len(messages) - 1; i >= 0 && remaining > 0; i-- {
		value := messages[i]
		if len(value) <= remaining {
			selected = append(selected, value)
			remaining -= len(value)
		} else {
			selected = append(selected, value[len(value)-remaining:])
			break
		}
	}
	// 反转回时间序。
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	output := make([]any, 0, len(selected)+1)
	for _, message := range selected {
		output = append(output, userMessageItem(message))
	}
	summaryText := "(no summary available)"
	if strings.TrimSpace(summary) != "" {
		summaryText = summaryPrefixText + "\n" + summary
	}
	output = append(output, userMessageItem(summaryText))
	return output
}

// extractChatResponseText 从非流式 chat 响应提取 assistant 文本。
func extractChatResponseText(body map[string]any) string {
	choices, _ := body["choices"].([]any)
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		message, ok := choice["message"].(map[string]any)
		if !ok {
			continue
		}
		if text, ok := message["content"].(string); ok {
			return text
		}
		if parts, ok := message["content"].([]any); ok {
			var buf strings.Builder
			for _, partRaw := range parts {
				if part, ok := partRaw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						buf.WriteString(text)
					}
				}
			}
			return buf.String()
		}
	}
	return ""
}

type chatUsage struct{ prompt, completion int64 }

func chatUsageTokens(body map[string]any) chatUsage {
	usageField, _ := body["usage"].(map[string]any)
	out := chatUsage{}
	if v, ok := usageField["prompt_tokens"].(float64); ok {
		out.prompt = int64(v)
	}
	if v, ok := usageField["completion_tokens"].(float64); ok {
		out.completion = int64(v)
	}
	return out
}

// compactionSnapshot 构造 v2 非流式响应壳。
func compactionSnapshot(model any, item map[string]any, status string) map[string]any {
	output := []any{}
	if item != nil {
		output = append(output, item)
	}
	return map[string]any{
		"id":         "resp_" + randomHexID(),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
		"usage":      nil,
	}
}

// writeCompactionSSE 写 v2 流式响应：created → output_item.done →
// completed → [DONE]（合成事件，带 sequence_number）。
func writeCompactionSSE(w http.ResponseWriter, model any, item map[string]any) {
	created := compactionSnapshot(model, nil, "in_progress")
	completed := compactionSnapshot(model, item, "completed")
	events := []struct {
		eventType string
		data      map[string]any
	}{
		{"response.created", map[string]any{"response": created}},
		{"response.output_item.done", map[string]any{"output_index": 0, "item": item}},
		{"response.completed", map[string]any{"response": completed}},
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for sequence, event := range events {
		payload := map[string]any{"type": event.eventType, "sequence_number": sequence}
		for k, v := range event.data {
			payload[k] = v
		}
		raw, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.eventType, raw)
	}
	io.WriteString(w, "data: [DONE]\n\n")
}

func randomHexID() string {
	return fmt.Sprintf("%x%x", time.Now().UnixNano(), time.Now().UnixMicro()%0xffff)
}
