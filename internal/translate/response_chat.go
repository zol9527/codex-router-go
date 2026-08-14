package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ChatToResponsesSSE 把 chat-completions 上游的 SSE 流增量翻译成
// Codex 期望的 Responses 事件流。
//
// 事件序列合同（Codex 端解析器要求）：
//
//	response.created
//	[reasoning item] output_item.added → reasoning_summary_part.added →
//	  reasoning_summary_text.delta* → …done → output_item.done
//	[message item]   output_item.added → content_part.added →
//	  output_text.delta* → output_text.done → content_part.done → output_item.done
//	[function_call]  output_item.added → function_call_arguments.delta* →
//	  function_call_arguments.done → output_item.done
//	response.completed（带完整 output 与 usage）
//	data: [DONE]
type ChatToResponsesSSE struct {
	responseID string
	model      string

	reasoning    *itemState
	message      *itemState
	functionCall map[int]*itemState
	nextIndex    int

	usage map[string]any
	done  bool

	// 补零替换的状态（见 responsesUsage）。
	estimatedInput       int
	substitutedInput     int
	observedPromptTokens int64

	// namespace 回写：模型发出的扁平 `<ns>__<tool>` 调用名还原为客户端
	// 派发的 {name, namespace} 形态（含 spawn_agent 白名单清洗、
	// create_thread 会话模型注入、整数 token 修复）。
	namespaceIndex *NamespaceIndex
	sessionModel   string
}

// WithNamespaceIndex 装载 namespace 还原索引（请求时 FlattenNamespaceTools
// 的产物；nil 表示该请求没有 namespace 工具，回写为空操作）。
func (t *ChatToResponsesSSE) WithNamespaceIndex(index *NamespaceIndex, sessionModel string) *ChatToResponsesSSE {
	t.namespaceIndex = index
	t.sessionModel = sessionModel
	return t
}

// WithEstimatedInputTokens 装载补零估算（仅大请求装载：小请求的零
// 无关紧要 —— 原实现的 "do not bother" 下限）。
func (t *ChatToResponsesSSE) WithEstimatedInputTokens(estimate int) *ChatToResponsesSSE {
	t.estimatedInput = estimate
	return t
}

// SubstitutedInputTokens 返回被替换进响应的估算值（0 = provider 自报）。
func (t *ChatToResponsesSSE) SubstitutedInputTokens() int { return t.substitutedInput }

// PromptTokens 返回 provider 报告的 prompt 数。
func (t *ChatToResponsesSSE) PromptTokens() int64 { return t.observedPromptTokens }

// OutputTokens / TotalTokens 从 usage 提取计量（usage 缺失为 0）。
func (t *ChatToResponsesSSE) OutputTokens() int64 {
	if v, ok := t.usage["completion_tokens"].(float64); ok {
		return int64(v)
	}
	return 0
}

func (t *ChatToResponsesSSE) TotalTokens() int64 {
	if v, ok := t.usage["total_tokens"].(float64); ok {
		return int64(v)
	}
	return 0
}

// HasContent 报告本流是否产出过客户端可行动的内容
// （输出文本或工具调用；纯 reasoning 不算 —— 空补全守卫的判定）。
func (t *ChatToResponsesSSE) HasContent() bool {
	if t.message != nil && t.message.text.Len() > 0 {
		return true
	}
	for _, state := range t.functionCall {
		if state != nil {
			return true
		}
	}
	return false
}

// OutputBuffer 累积 Feed 产出的完整 SSE 块（守卫模式：
// 选完尝试再整段写出，写流阶段复用同一缓冲）。
type OutputBuffer struct {
	buf []byte
}

// Write 记录一段翻译输出。
func (e *OutputBuffer) Write(chunk []byte) { e.buf = append(e.buf, chunk...) }

// Bytes 返回累积的全部字节。
func (e *OutputBuffer) Bytes() []byte { return e.buf }

// TranslateNonStreamChatWith 用现成翻译器处理非流式响应
// （estimate 已在翻译器上装载）。
func TranslateNonStreamChatWith(body map[string]any, t *ChatToResponsesSSE) map[string]any {
	if id, ok := body["id"].(string); ok && id != "" {
		t.responseID = id
	}
	choices, _ := body["choices"].([]any)
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if message, ok := choice["message"].(map[string]any); ok {
			t.feedDelta(message)
		}
	}
	if usage, ok := body["usage"].(map[string]any); ok {
		t.usage = usage
	}
	t.close()
	response := t.responseShell("completed")
	response["output"] = t.completedOutput()
	response["usage"] = t.responsesUsage(t.usage)
	return response
}

type itemState struct {
	kind        string // "reasoning" | "message" | "function_call"
	itemID      string
	callID      string
	name        string
	outputIndex int
	arguments   strings.Builder
	text        strings.Builder
	added       bool
	closed      bool
}

var nowFunc = time.Now().Unix
var idMu sync.Mutex
var idCounter int64

// randomID 生成进程内唯一的短 id（事件 id 只需唯一，不需不可预测）。
func randomID() string {
	idMu.Lock()
	idCounter++
	n := idCounter
	idMu.Unlock()
	return fmt.Sprintf("%x%x", nowFunc(), n)
}

// NewChatToResponsesSSE 创建翻译器。
func NewChatToResponsesSSE(responseID, model string) *ChatToResponsesSSE {
	return &ChatToResponsesSSE{
		responseID:   orDefault(responseID, "resp_"+randomID()),
		model:        model,
		functionCall: map[int]*itemState{},
	}
}

// Created 返回流开头的 response.created 事件块。
func (t *ChatToResponsesSSE) Created() []byte {
	return renderEvents([]string{eventJSON("response.created", map[string]any{
		"response": t.responseShell("in_progress"),
	})})
}

// Feed 处理上游 SSE 的一个 data 载荷（不含 "data: " 前缀），
// 返回需要立即写给 Codex 的完整 SSE 块。"[DONE]" 触发收尾。
func (t *ChatToResponsesSSE) Feed(data string) []byte {
	if data == "[DONE]" {
		return t.close()
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil // 非 JSON 的 data 行丢弃
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		t.usage = usage
	}
	choices, _ := chunk["choices"].([]any)
	var payloads []string
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			if message, ok := choice["message"].(map[string]any); ok {
				delta = message
			} else {
				continue
			}
		}
		payloads = append(payloads, t.feedDelta(delta)...)
	}
	return renderEvents(payloads)
}

// TranslateNonStreamChat 把一整个非流式 chat 响应翻译成 Responses JSON。
func TranslateNonStreamChat(body map[string]any, responseID, model string) map[string]any {
	t := NewChatToResponsesSSE(responseID, model)
	if id, ok := body["id"].(string); ok && id != "" {
		t.responseID = id
	}
	choices, _ := body["choices"].([]any)
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if message, ok := choice["message"].(map[string]any); ok {
			t.feedDelta(message)
		}
	}
	if usage, ok := body["usage"].(map[string]any); ok {
		t.usage = usage
	}
	t.close()
	response := t.responseShell("completed")
	response["output"] = t.completedOutput()
	response["usage"] = t.responsesUsage(t.usage)
	return response
}

// feedDelta 消费一个 chat delta（或非流式 message），返回增量事件载荷。
func (t *ChatToResponsesSSE) feedDelta(delta map[string]any) []string {
	var payloads []string

	// 思维链：GLM 系用 reasoning_content，部分转售商用 reasoning。
	reasoning := deltaText(delta["reasoning_content"])
	if reasoning == "" {
		reasoning = deltaText(delta["reasoning"])
	}
	if reasoning != "" {
		if t.reasoning == nil {
			t.reasoning = t.newItem("reasoning")
			payloads = append(payloads,
				eventJSON("response.output_item.added", map[string]any{
					"output_index": t.reasoning.outputIndex,
					"item":         t.reasoning.reasoningItem(),
				}),
				eventJSON("response.reasoning_summary_part.added", map[string]any{
					"item_id":       t.reasoning.itemID,
					"output_index":  t.reasoning.outputIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": ""},
				}))
		}
		t.reasoning.text.WriteString(reasoning)
		payloads = append(payloads, eventJSON("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       t.reasoning.itemID,
			"output_index":  t.reasoning.outputIndex,
			"summary_index": 0,
			"delta":         reasoning,
		}))
	}

	// 可见文本。
	if content := deltaText(delta["content"]); content != "" {
		if t.message == nil {
			t.message = t.newItem("message")
			payloads = append(payloads,
				eventJSON("response.output_item.added", map[string]any{
					"output_index": t.message.outputIndex,
					"item":         t.message.messageItem(),
				}),
				eventJSON("response.content_part.added", map[string]any{
					"item_id":       t.message.itemID,
					"output_index":  t.message.outputIndex,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": ""},
				}))
		}
		t.message.text.WriteString(content)
		payloads = append(payloads, eventJSON("response.output_text.delta", map[string]any{
			"item_id":       t.message.itemID,
			"output_index":  t.message.outputIndex,
			"content_index": 0,
			"delta":         content,
		}))
	}

	// 工具调用：按 chat tool_calls[].index 维持多个并行 item。
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			chatIndex := 0
			if v, ok := call["index"].(float64); ok {
				chatIndex = int(v)
			}
			state := t.functionCall[chatIndex]
			if state == nil {
				state = t.newItem("function_call")
				t.functionCall[chatIndex] = state
			}
			if id, ok := call["id"].(string); ok && id != "" && state.callID == "" {
				state.callID = id
			}
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if name, ok := fn["name"].(string); ok && name != "" && state.name == "" {
				state.name = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				if !state.added {
					payloads = append(payloads, eventJSON("response.output_item.added", map[string]any{
						"output_index": state.outputIndex,
						"item":         t.functionCallItem(state, ""),
					}))
					state.added = true
				}
				state.arguments.WriteString(args)
				payloads = append(payloads, eventJSON("response.function_call_arguments.delta", map[string]any{
					"item_id":      state.itemID,
					"output_index": state.outputIndex,
					"delta":        args,
				}))
			}
		}
	}
	return payloads
}

// newItem 分配 output index 与 item id。
// callID 不在此预设：chat 上游的 tool_calls[].id 通常随首个增量到达，
// 预设的占位值会挡住它 —— 真实 id 优先，缺失才在 item 构造时派生。
func (t *ChatToResponsesSSE) newItem(kind string) *itemState {
	prefix := map[string]string{
		"reasoning": "rs_", "message": "msg_", "function_call": "fc_",
	}[kind]
	state := &itemState{
		kind:        kind,
		itemID:      prefix + randomID(),
		outputIndex: t.nextIndex,
		added:       kind != "function_call",
	}
	t.nextIndex++
	return state
}

// close 关闭全部进行中 items，产出 response.completed 与 [DONE]。
func (t *ChatToResponsesSSE) close() []byte {
	if t.done {
		return nil
	}
	t.done = true
	var payloads []string
	payloads = append(payloads, t.closeReasoning()...)
	payloads = append(payloads, t.closeMessage()...)
	for i := 0; i < len(t.functionCall); i++ {
		payloads = append(payloads, t.closeFunctionCall(i)...)
	}
	response := t.responseShell("completed")
	response["output"] = t.completedOutput()
	response["usage"] = t.responsesUsage(t.usage)
	payloads = append(payloads, eventJSON("response.completed", map[string]any{
		"response": response,
	}))
	return renderEvents(append(payloads, "[DONE]"))
}

func (t *ChatToResponsesSSE) closeReasoning() []string {
	s := t.reasoning
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	text := s.text.String()
	return []string{
		eventJSON("response.reasoning_summary_text.done", map[string]any{
			"item_id":       s.itemID,
			"output_index":  s.outputIndex,
			"summary_index": 0,
			"text":          text,
		}),
		eventJSON("response.reasoning_summary_part.done", map[string]any{
			"item_id":       s.itemID,
			"output_index":  s.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": text},
		}),
		eventJSON("response.output_item.done", map[string]any{
			"output_index": s.outputIndex,
			"item":         s.reasoningDoneItem(text),
		}),
	}
}

func (t *ChatToResponsesSSE) closeMessage() []string {
	s := t.message
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	text := s.text.String()
	if text == "" {
		return nil // 从未产出文本的 message item 不进 output
	}
	return []string{
		eventJSON("response.output_text.done", map[string]any{
			"item_id":       s.itemID,
			"output_index":  s.outputIndex,
			"content_index": 0,
			"text":          text,
		}),
		eventJSON("response.content_part.done", map[string]any{
			"item_id":       s.itemID,
			"output_index":  s.outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": text},
		}),
		eventJSON("response.output_item.done", map[string]any{
			"output_index": s.outputIndex,
			"item":         s.messageDoneItem(text),
		}),
	}
}

func (t *ChatToResponsesSSE) closeFunctionCall(chatIndex int) []string {
	s := t.functionCall[chatIndex]
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	args := s.arguments.String()
	if args == "" {
		args = "{}"
	}
	var payloads []string
	if !s.added {
		payloads = append(payloads, eventJSON("response.output_item.added", map[string]any{
			"output_index": s.outputIndex,
			"item":         t.functionCallItem(s, ""),
		}))
	}
	payloads = append(payloads,
		eventJSON("response.function_call_arguments.done", map[string]any{
			"item_id":      s.itemID,
			"output_index": s.outputIndex,
			"arguments":    args,
		}),
		eventJSON("response.output_item.done", map[string]any{
			"output_index": s.outputIndex,
			"item":         t.functionCallItem(s, args),
		}))
	return payloads
}

// completedOutput 汇总完整 output 数组（completed 事件与非流式响应共用）。
func (t *ChatToResponsesSSE) completedOutput() []any {
	var output []any
	if t.reasoning != nil && t.reasoning.text.Len() > 0 {
		output = append(output, t.reasoning.reasoningDoneItem(t.reasoning.text.String()))
	}
	if t.message != nil && t.message.text.Len() > 0 {
		output = append(output, t.message.messageDoneItem(t.message.text.String()))
	}
	for i := 0; i < len(t.functionCall); i++ {
		if s := t.functionCall[i]; s != nil {
			args := s.arguments.String()
			if args == "" {
				args = "{}"
			}
			output = append(output, t.functionCallItem(s, args))
		}
	}
	return output
}

func (t *ChatToResponsesSSE) responseShell(status string) map[string]any {
	return map[string]any{
		"id":         t.responseID,
		"object":     "response",
		"created_at": nowFunc(),
		"status":     status,
		"model":      t.model,
		"output":     []any{},
	}
}

// responsesUsage 把 chat usage 字段名换成 Responses 字段名。
// Prompt-token 补零替换（#95）在此生效：上游对大 prompt 报
// input_tokens: 0 会让 Codex 永不压缩、会话撑爆窗口。替换只落在
// 显式零上、estimate 只高不低（压缩阈值有 14% 余量），替换事实通过
// SubstitutedInputTokens 单独暴露 —— telemetry 永远保留 provider 原值。
func (t *ChatToResponsesSSE) responsesUsage(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	out := map[string]any{}
	promptTokens, _ := usage["prompt_tokens"].(float64)
	t.observedPromptTokens = int64(promptTokens)
	if promptTokens == 0 && t.estimatedInput > 0 {
		out["input_tokens"] = t.estimatedInput
		t.substitutedInput = t.estimatedInput
		if completion, ok := usage["completion_tokens"].(float64); ok {
			out["output_tokens"] = completion
			out["total_tokens"] = float64(t.estimatedInput) + completion
		} else {
			out["total_tokens"] = float64(t.estimatedInput)
		}
	} else {
		if v, ok := usage["prompt_tokens"]; ok {
			out["input_tokens"] = v
		}
		if v, ok := usage["completion_tokens"]; ok {
			out["output_tokens"] = v
		}
		if v, ok := usage["total_tokens"]; ok {
			out["total_tokens"] = v
		}
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		out["input_tokens_details"] = details
	}
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		out["output_tokens_details"] = details
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- item 形状构造 ----

func (s *itemState) reasoningItem() map[string]any {
	return map[string]any{"type": "reasoning", "id": s.itemID, "summary": []any{}}
}

func (s *itemState) reasoningDoneItem(text string) map[string]any {
	return map[string]any{
		"type": "reasoning", "id": s.itemID,
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
}

func (s *itemState) messageItem() map[string]any {
	return map[string]any{
		"type": "message", "id": s.itemID, "role": "assistant", "content": []any{},
	}
}

func (s *itemState) messageDoneItem(text string) map[string]any {
	return map[string]any{
		"type": "message", "id": s.itemID, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text}},
	}
}

func (t *ChatToResponsesSSE) functionCallItem(s *itemState, args string) map[string]any {
	item := map[string]any{
		"type": "function_call", "id": s.itemID,
		"call_id":   orDefault(s.callID, "call_"+s.itemID),
		"name":      s.name,
		"arguments": args, "status": "completed",
	}
	if t.namespaceIndex != nil {
		if rewritten := t.namespaceIndex.RewriteFunctionCallItem(item, t.sessionModel); rewritten != nil {
			return rewritten
		}
	}
	return item
}

// ---- SSE 基础设施 ----

// eventJSON 序列化一个事件载荷（自动带 type 字段）。
func eventJSON(eventType string, payload map[string]any) string {
	payload["type"] = eventType
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(raw)
}

// renderEvents 把载荷列表渲染成完整 SSE 块字节。
func renderEvents(payloads []string) []byte {
	var buf strings.Builder
	for _, payload := range payloads {
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			buf.WriteString("data: [DONE]\n\n")
			continue
		}
		// event: 行从载荷的 type 提取 —— Codex 按 data 解析，
		// 规范双写对其他客户端更稳。
		eventType := ""
		var probe map[string]any
		if json.Unmarshal([]byte(payload), &probe) == nil {
			if t, ok := probe["type"].(string); ok {
				eventType = t
			}
		}
		if eventType != "" {
			buf.WriteString("event: " + eventType + "\n")
		}
		buf.WriteString("data: " + payload + "\n\n")
	}
	return []byte(buf.String())
}

// deltaText 提取字符串形态的 delta 文本（部分网关发数组形态，一并处理）。
func deltaText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var buf strings.Builder
		for _, raw := range v {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					buf.WriteString(text)
				}
			}
		}
		return buf.String()
	default:
		return ""
	}
}
