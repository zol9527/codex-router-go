package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/translate"
	"github.com/loyd/codex-router/internal/domain/usage"
	"github.com/loyd/codex-router/internal/engine/routing"
)

// routed compaction，移植自 router.mjs 的 compaction 族函数。
//
// 压缩是把整段对话重放给同一个 provider 生成交接摘要：
//   - v1（POST /responses/compact）：返回 {output: [尾预算内的 user 消息
//     + 摘要消息]}，Codex 拿去续会话；
//   - v2（input 尾部 compaction_trigger）：返回 kcr1: base64 摘要的
//     compaction item（JSON 或合成 SSE，按请求的 stream 字段）。

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
	credential string, v2 bool, started time.Time) {

	runner := *s.routeRunner
	runner.Recorder = s.opt.Usage
	runner.RateLimits = s.opt.RateLimits
	runner.Client = s.client
	runner.Idle = s.upstreamIdle
	result, ok := runner.RunCompaction(routing.Request{
		Context: r.Context(), Payload: payload, Model: model, Provider: provider,
		Credential: credential, Sink: responseSink{w: w}, Header: r.Header,
		Started: started,
	})
	if !ok {
		return
	}
	providerID := s.registry().CanonicalProviderID(provider.ID)
	if !v2 {
		writeJSON(w, http.StatusOK, map[string]any{"output": compactOutput(payload["input"], result.Summary)})
		s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 200,
			DurationMs:  time.Since(started).Milliseconds(),
			InputTokens: result.PromptTokens, OutputTokens: result.CompletionTokens})
		return
	}
	item := map[string]any{
		"type": "compaction", "id": "cmp_" + randomHexID(),
		"encrypted_content": encodeSummaryText(result.Summary),
	}
	if stream, _ := payload["stream"].(bool); !stream {
		writeJSON(w, http.StatusOK, compactionSnapshot(payload["model"], item, "completed"))
	} else {
		writeCompactionSSE(w, payload["model"], item)
	}
	s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 200,
		DurationMs:  time.Since(started).Milliseconds(),
		InputTokens: result.PromptTokens, OutputTokens: result.CompletionTokens})
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
