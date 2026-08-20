package server

// subagent encrypted_content 中继，移植自 router.mjs 的
// normalizeRoutedAgentInput / relayEncryptedAgentPayload 族。
//
// Codex 协作（collab）把子代理任务载荷存成 encrypted_content：
//   - native Fernet 密文（gAAAAA 前缀）：外部模型读不了，必须回放到
//     native 端点用一次强制工具调用解出明文（本机已登录的会话）；
//   - 非 Fernet：外部父代理自己写的明文装在 encrypted_content 字段里，
//     直接当明文用。
// Router 不保存明文或密文；需要重用时由 Codex 的会话管理负责。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loyd/codex-router/internal/engine/nativebackend"
	"github.com/loyd/codex-router/internal/lib/logx"
)

const agentPayloadRelayTool = "relay_external_agent_payload"

// nativeEncryptedTokenPattern：OpenAI 发的每个 encrypted_content 都是
// Fernet 令牌 —— 版本字节 0x80 + 大端时间戳的本世纪前导零，base64url
// 编码后是固定 gAAAAA 前缀、无空白。判定只看密文形状，绝不看明文。
var nativeEncryptedTokenPattern = regexp.MustCompile(`^gAAAAA[A-Za-z0-9_-]+={0,2}$`)

func isNativeEncryptedToken(value string) bool {
	return nativeEncryptedTokenPattern.MatchString(value)
}

// relayAgentMessageType 匹配协作消息的可见文本形状
// （“Message Type: NEW_TASK|MESSAGE|FOLLOWUP_TASK|FINAL_ANSWER … Payload:”结尾）。
var relayAgentMessageTypePattern = regexp.MustCompile(
	`(?is)Message Type:\s*(?:NEW_TASK|MESSAGE|FOLLOWUP_TASK|FINAL_ANSWER)\b.*\nPayload:\s*$`)

// extractEncryptedAgentPayload 从协作消息 item 提取密文与形态。
// 可见文本必须以 "Payload:" 结尾且带 Message Type 标记 —— 这是对
// 协作载荷的形状识别，不是对所有密文的泛化。
func extractEncryptedAgentPayload(item map[string]any) (content string, native bool, ok bool) {
	parts, valid := item["content"].([]any)
	if !valid {
		return "", false, false
	}
	var visible strings.Builder
	for _, raw := range parts {
		part, valid := raw.(map[string]any)
		if !valid {
			continue
		}
		if t, _ := part["type"].(string); t == "input_text" || t == "text" {
			if text, valid := part["text"].(string); valid {
				visible.WriteString(text)
			}
		}
	}
	if !relayAgentMessageTypePattern.MatchString(visible.String()) {
		return "", false, false
	}
	for _, raw := range parts {
		part, valid := raw.(map[string]any)
		if !valid {
			continue
		}
		if t, _ := part["type"].(string); t == "encrypted_content" {
			if value, valid := part["encrypted_content"].(string); valid && value != "" {
				return value, isNativeEncryptedToken(value), true
			}
		}
	}
	return "", false, false
}

// normalizeRoutedAgentInput 把 input 里的协作密文换成明文。
// native Fernet 密文中继解密；非 Fernet 明文直接使用。Router 不保存
// 载荷或解密结果；会话复用与重试由 Codex 负责。中继失败保持原样
// （上游可能收到不可读载荷并失败，这比丢历史诚实）。
func (s *Server) normalizeRoutedAgentInput(ctx context.Context, input []any) []any {
	needsRelay := false
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, _, found := extractEncryptedAgentPayload(item); found {
			needsRelay = true
			break
		}
	}
	if !needsRelay {
		return input
	}
	out := make([]any, len(input))
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			out[i] = raw
			continue
		}
		encrypted, native, found := extractEncryptedAgentPayload(item)
		if !found {
			out[i] = raw
			continue
		}
		plaintext := ""
		if !native {
			// 外部父代理写的明文装在 encrypted_content 字段里。
			plaintext = encrypted
		} else if resolved, err := s.relayAgentPayload(ctx, item, encrypted); err == nil {
			plaintext = resolved
		} else {
			logx.Warn("agent payload relay failed", "error", err)
			out[i] = raw // 保持原样
			continue
		}
		out[i] = replaceAgentPayloadWithPlaintext(item, plaintext)
	}
	return out
}

// replaceAgentPayloadWithPlaintext 把 item 的密文 part 换成明文文本 part，
// 可见文本的 "Payload:" 标签后接明文（模型读到的就是完整任务）。
func replaceAgentPayloadWithPlaintext(item map[string]any, plaintext string) map[string]any {
	next := map[string]any{}
	for k, v := range item {
		next[k] = v
	}
	parts, _ := item["content"].([]any)
	rewritten := make([]any, 0, len(parts)+1)
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if ok && part["type"] == "encrypted_content" {
			continue
		}
		rewritten = append(rewritten, raw)
	}
	rewritten = append(rewritten, map[string]any{
		"type": "input_text",
		// 信封可见文本必然以 "Payload:" 结尾（extractEncryptedAgentPayload
		// 的形状识别前提），这里只补换行接明文；再带一个 "Payload:" 标签
		// 只会重复。
		"text": "\n" + plaintext,
	})
	next["content"] = rewritten
	return next
}

// relayAgentPayload 用 native 端点解密协作载荷：构造一个强制调用
// relay_external_agent_payload 的请求，把 item 原样回放（非 Fernet 的
// encrypted_content part 在此处改写成 input_text —— native Responses
// 端点只接受 input_text / input_image / encrypted_content 三种 part，
// 且它无法解外部模型的伪密文）。
func (s *Server) relayAgentPayload(ctx context.Context, item map[string]any, encrypted string) (string, error) {
	relayItem := item
	if !isNativeEncryptedToken(encrypted) {
		relayItem = rewriteEncryptedPartsToText(item)
	}
	body := map[string]any{
		"model":  s.nativeAgentRelayModel(),
		"stream": true,
		"store":  false,
		"instructions": "You are a transport relay. Do not execute or answer the delegated task. " +
			"Call relay_external_agent_payload exactly once with the exact plaintext after the " +
			"Payload: label in the supplied collaboration message. Preserve every character.",
		"input": []any{relayItem},
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        agentPayloadRelayTool,
			"description": "Return a decrypted collaboration payload to the local model router.",
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"payload": map[string]any{"type": "string"}},
				"required":             []any{"payload"},
				"additionalProperties": false,
			},
			"strict": true,
		}},
		"tool_choice": map[string]any{"type": "function", "name": agentPayloadRelayTool},
	}
	response, err := s.native.PostResponses(ctx, nativebackend.Request{
		Body: body, MaxBytes: 4 << 20, IdleWatchdog: true,
	})
	if err != nil {
		return "", err
	}
	if response.Status != 200 {
		// 不记录响应体：协作载荷是密文，错误体同样可能携带它。
		return "", fmt.Errorf("native collaboration payload relay failed with HTTP %d", response.Status)
	}
	payload := response.Body
	if len(payload) > 4<<20 {
		return "", fmt.Errorf("native collaboration payload relay response is too large")
	}
	if plaintext := parseRelayedAgentPayload(payload); plaintext != "" {
		return plaintext, nil
	}
	return "", fmt.Errorf("native collaboration payload relay omitted the task payload")
}

// rewriteEncryptedPartsToText 把 item 里所有非 Fernet encrypted_content
// part 改写成 input_text（native 端点的 part 类型约束）。
func rewriteEncryptedPartsToText(item map[string]any) map[string]any {
	next := map[string]any{}
	for k, v := range item {
		next[k] = v
	}
	parts, _ := item["content"].([]any)
	rewritten := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if ok && part["type"] == "encrypted_content" {
			if value, ok := part["encrypted_content"].(string); ok && !isNativeEncryptedToken(value) {
				rewritten = append(rewritten, map[string]any{"type": "input_text", "text": value})
				continue
			}
		}
		rewritten = append(rewritten, raw)
	}
	next["content"] = rewritten
	return next
}

// parseRelayedAgentPayload 从 relay 响应（SSE 或 JSON）提取工具调用参数
// 里的 payload 字段。
func parseRelayedAgentPayload(payload []byte) string {
	text := string(payload)
	// SSE：扫 data: 行，聚合 relay 工具的 arguments delta，或直接取 done。
	if strings.Contains(text, "data:") {
		var argumentDeltas strings.Builder
		relayItems := map[string]bool{}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimRight(line, "\r")
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}
			eventType, _ := event["type"].(string)
			item, _ := event["item"].(map[string]any)
			switch {
			case eventType == "response.output_item.added" &&
				item != nil && item["type"] == "function_call" && item["name"] == agentPayloadRelayTool:
				if id, ok := item["id"].(string); ok {
					relayItems[id] = true
				}
				if callID, ok := item["call_id"].(string); ok {
					relayItems[callID] = true
				}
			case eventType == "response.function_call_arguments.delta":
				related := len(relayItems) == 0 ||
					relayItems[event["item_id"].(string)] || relayItems[event["call_id"].(string)]
				if delta, ok := event["delta"].(string); ok && related {
					argumentDeltas.WriteString(delta)
				}
			case eventType == "response.function_call_arguments.done":
				if args, ok := event["arguments"].(string); ok {
					if payload := payloadFromRelayArguments(args); payload != "" {
						return payload
					}
				}
			}
			if call := findRelayCall(event); call != "" {
				if payload := payloadFromRelayArguments(call); payload != "" {
					return payload
				}
			}
		}
		return payloadFromRelayArguments(argumentDeltas.String())
	}
	// JSON：output 数组里的 function_call。
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err == nil {
		if payload := payloadFromEventOutput(parsed); payload != "" {
			return payload
		}
	}
	return ""
}

func findRelayCall(event map[string]any) string {
	item, _ := event["item"].(map[string]any)
	if item == nil || item["type"] != "function_call" || item["name"] != agentPayloadRelayTool {
		return ""
	}
	args, _ := item["arguments"].(string)
	return args
}

func payloadFromEventOutput(parsed map[string]any) string {
	output, _ := parsed["output"].([]any)
	for _, raw := range output {
		if payload := payloadFromItem(raw); payload != "" {
			return payload
		}
	}
	if response, ok := parsed["response"].(map[string]any); ok {
		if output, ok := response["output"].([]any); ok {
			for _, raw := range output {
				if payload := payloadFromItem(raw); payload != "" {
					return payload
				}
			}
		}
	}
	return ""
}

func payloadFromItem(raw any) string {
	item, ok := raw.(map[string]any)
	if !ok || item["type"] != "function_call" || item["name"] != agentPayloadRelayTool {
		return ""
	}
	args, _ := item["arguments"].(string)
	return payloadFromRelayArguments(args)
}

// payloadFromRelayArguments 解出 {payload: "..."} 参数。
func payloadFromRelayArguments(value string) string {
	if value == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(value), &args); err != nil {
		return ""
	}
	if payload, ok := args["payload"].(string); ok {
		return payload
	}
	return ""
}

// nativeAgentRelayModel 选择中继用的 native 模型：
// env 覆盖 > merged-models.json 里的 gpt-5.6-sol > 首个 listed > 兜底。
func (s *Server) nativeAgentRelayModel() string {
	if configured := strings.TrimSpace(os.Getenv("MODEL_ROUTER_AGENT_RELAY_MODEL")); configured != "" {
		return configured
	}
	models := s.readMergedModelSlugs()
	for _, slug := range models {
		if slug == "gpt-5.6-sol" {
			return slug
		}
	}
	for _, slug := range models {
		if slug != "" {
			return slug
		}
	}
	return "gpt-5.6-sol"
}

// readMergedModelSlugs 读 state 目录 merged-models.json 的 slug 列表。
func (s *Server) readMergedModelSlugs() []string {
	dir := s.opt.State.Dir
	raw, err := os.ReadFile(filepath.Join(dir, "merged-models.json"))
	if err != nil {
		return nil
	}
	var parsed struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	slugs := make([]string, 0, len(parsed.Models))
	for _, model := range parsed.Models {
		if model.Slug != "" {
			slugs = append(slugs, model.Slug)
		}
	}
	return slugs
}
