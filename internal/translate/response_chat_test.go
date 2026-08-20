package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func feedAll(t *testing.T, tr *ChatToResponsesSSE, chunks ...string) []byte {
	t.Helper()
	var out []byte
	for _, chunk := range chunks {
		out = append(out, tr.Feed(chunk)...)
	}
	return out
}

func eventTypes(t *testing.T, raw []byte) []string {
	t.Helper()
	var types []string
	for _, block := range strings.Split(string(raw), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					continue
				}
				typ, _ := obj(t, payload)["type"].(string)
				types = append(types, typ)
			}
		}
	}
	return types
}

// 黄金序列：reasoning + 文本 + 工具调用 + usage 的完整翻译。
func TestSSETranslationGoldenSequence(t *testing.T) {
	tr := NewChatToResponsesSSE("chat-1", "glm-5.3")
	var out []byte
	out = append(out, tr.Created()...)
	out = append(out, feedAll(t, tr,
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"Thinking..."}}]}`,
		`{"choices":[{"delta":{"content":"Hello "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"shell","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`[DONE]`,
	)...)

	types := eventTypes(t, out)
	wantOrder := []string{
		"response.created",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done", // reasoning
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // message
		"response.function_call_arguments.done",
		"response.output_item.done", // function_call
		"response.completed",
	}
	if len(types) != len(wantOrder) {
		t.Fatalf("event count mismatch:\ngot  %v\nwant %v", types, wantOrder)
	}
	for i := range types {
		if types[i] != wantOrder[i] {
			t.Fatalf("event[%d] = %s, want %s\nfull: %v", i, types[i], wantOrder[i], types)
		}
	}

	// completed 事件携带完整 output 与 usage。
	var completed map[string]any
	for _, block := range strings.Split(string(out), "\n\n") {
		if strings.Contains(block, "response.completed") {
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(line, "data: ") {
					completed = obj(t, strings.TrimPrefix(line, "data: "))
				}
			}
		}
	}
	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("completed output should carry 3 items, got %d", len(output))
	}
	items := []map[string]any{
		output[0].(map[string]any), output[1].(map[string]any), output[2].(map[string]any),
	}
	if items[0]["type"] != "reasoning" || items[1]["type"] != "message" || items[2]["type"] != "function_call" {
		t.Errorf("output item order wrong: %v %v %v", items[0]["type"], items[1]["type"], items[2]["type"])
	}
	msgContent := items[1]["content"].([]any)
	if msgContent[0].(map[string]any)["text"] != "Hello world" {
		t.Errorf("message text should be concatenated, got %v", msgContent)
	}
	fn := items[2]
	if fn["name"] != "shell" || fn["arguments"] != `{"cmd":"ls"}` || fn["call_id"] != "call_9" {
		t.Errorf("function_call item wrong: %+v", fn)
	}
	if id, _ := fn["id"].(string); !strings.HasPrefix(id, "fc_") {
		t.Errorf("function_call id = %q, want fc_ prefix", id)
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) {
		t.Errorf("usage mapping wrong: %+v", usage)
	}
	if !strings.Contains(string(out), "data: [DONE]") {
		t.Error("stream must end with [DONE]")
	}
}

// 多个并行工具调用按 chat index 各自成 item。
func TestSSEMultipleToolCalls(t *testing.T) {
	tr := NewChatToResponsesSSE("", "m")
	out := feedAll(t, tr,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","type":"function","function":{"name":"b","arguments":"{}"}}]}}]}`,
		`[DONE]`)
	if strings.Count(string(out), `"type":"function_call"`) < 4 {
		// added/done 各两次 + completed output 两次 = 至少 4 处 function_call 形状
		t.Errorf("expected two function_call items, output:\n%s", out)
	}
	var completed map[string]any
	for _, block := range strings.Split(string(out), "\n\n") {
		if strings.Contains(block, "response.completed") {
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(line, "data: ") {
					completed = obj(t, strings.TrimPrefix(line, "data: "))
				}
			}
		}
	}
	output := completed["response"].(map[string]any)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected 2 function calls in completed output, got %d", len(output))
	}
	if output[0].(map[string]any)["name"] != "a" || output[1].(map[string]any)["name"] != "b" {
		t.Errorf("function call order wrong: %+v", output)
	}
}

// 非流式 chat 响应整体翻译。
func TestNonStreamTranslation(t *testing.T) {
	body := obj(t, `{
		"id": "chatcmpl-1",
		"choices": [{"index":0,"message":{"role":"assistant","content":"Direct answer"},"finish_reason":"stop"}],
		"usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
	}`)
	response := TranslateNonStreamChat(body, "", "glm-5.3")
	if response["id"] != "chatcmpl-1" {
		t.Errorf("id should be preserved, got %v", response["id"])
	}
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(output))
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" {
		t.Errorf("expected message item, got %v", item["type"])
	}
	if item["content"].([]any)[0].(map[string]any)["text"] != "Direct answer" {
		t.Errorf("text mismatch: %v", item["content"])
	}
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(7) {
		t.Errorf("usage mapping wrong: %+v", usage)
	}
}

// 只有 reasoning 没有 output：completed output 为空但事件序列完整
// （空补全守卫在 M3 依赖这个形态做判定）。
func TestSSEReasoningOnly(t *testing.T) {
	tr := NewChatToResponsesSSE("", "m")
	out := feedAll(t, tr,
		`{"choices":[{"delta":{"reasoning_content":"hm"}}]}`,
		`[DONE]`)
	if !strings.Contains(string(out), "response.reasoning_summary_text.delta") {
		t.Error("reasoning delta missing")
	}
	var completed map[string]any
	for _, block := range strings.Split(string(out), "\n\n") {
		if strings.Contains(block, "response.completed") {
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(line, "data: ") {
					completed = obj(t, strings.TrimPrefix(line, "data: "))
				}
			}
		}
	}
	// reasoning item 应出现在 completed output 里（有内容）。
	if len(completed["response"].(map[string]any)["output"].([]any)) != 1 {
		t.Errorf("reasoning item should be in output, got %v", completed["response"])
	}
}

// content 数组形态的 delta（部分网关）。
func TestDeltaArrayContent(t *testing.T) {
	tr := NewChatToResponsesSSE("", "m")
	out := feedAll(t, tr,
		`{"choices":[{"delta":{"content":[{"type":"text","text":"chunk"}]}}]}`,
		`[DONE]`)
	if !strings.Contains(string(out), `output_text.delta`) {
		t.Errorf("array content delta should translate, got:\n%s", out)
	}
}

// 恶意/畸形 data 行不会打断翻译器。
func TestMalformedDataIgnored(t *testing.T) {
	tr := NewChatToResponsesSSE("", "m")
	out := feedAll(t, tr,
		`not json at all`,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`[DONE]`)
	if !strings.Contains(string(out), "ok") {
		t.Error("valid chunks after malformed one must still translate")
	}
	_ = json.Marshal
}

// null 防御（2026-08-19 实发）：上游偶发把 usage 字段或 details 子项
// 报成 null（deepseek 短输出时 reasoning_tokens=null），null 落进
// response.completed 会让 Codex 整个事件解析失败并全量重试。
// 数值照常透传，null 一律丢弃。
func TestUsageNullDefense(t *testing.T) {
	// SSE 路径：最终 chunk 携带含 null 的 usage。
	tr := NewChatToResponsesSSE("", "m")
	out := feedAll(t, tr,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"usage":{"prompt_tokens":9,"completion_tokens":null,"total_tokens":null,`+
			`"prompt_tokens_details":{"cached_tokens":null},`+
			`"completion_tokens_details":{"reasoning_tokens":null,"text_tokens":4}}}`,
		`[DONE]`)
	var completed map[string]any
	for _, block := range strings.Split(string(out), "\n\n") {
		if strings.Contains(block, "response.completed") {
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(line, "data: ") {
					completed = obj(t, strings.TrimPrefix(line, "data: "))
				}
			}
		}
	}
	usage := completed["response"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"] != float64(9) {
		t.Errorf("numeric fields should be preserved: %+v", usage)
	}
	for _, bad := range []string{"output_tokens", "total_tokens", "input_tokens_details"} {
		if _, exists := usage[bad]; exists {
			t.Errorf("%s must be dropped when null, got: %+v", bad, usage)
		}
	}
	details := usage["output_tokens_details"].(map[string]any)
	if details["text_tokens"] != float64(4) || len(details) != 1 {
		t.Errorf("details should keep only numeric subfields, got: %+v", details)
	}

	// 非流式路径共用 responsesUsage，同样防御；序列化结果不允许出现 null 字面量。
	body := obj(t, `{"choices":[{"message":{"role":"assistant","content":"x"}}],`+
		`"usage":{"prompt_tokens":null,"completion_tokens":2,"total_tokens":null}}`)
	resp := TranslateNonStreamChat(body, "", "m")
	usage2 := resp["usage"].(map[string]any)
	if _, exists := usage2["input_tokens"]; exists {
		t.Errorf("null prompt_tokens must not surface as input_tokens: %+v", usage2)
	}
	if usage2["output_tokens"] != float64(2) {
		t.Errorf("numeric completion_tokens should survive: %+v", usage2)
	}
	raw, _ := json.Marshal(usage2)
	if strings.Contains(string(raw), "null") {
		t.Errorf("serialized usage must not contain null: %s", raw)
	}
}
