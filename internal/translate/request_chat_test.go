package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func obj(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, raw)
	}
	return out
}

// 请求方向：Codex Responses 请求的典型形态完整翻译。
func TestTranslateToChatRoundTrip(t *testing.T) {
	input := obj(t, `{
		"model": "zai-coding-glm-5-3",
		"instructions": "You are a coding agent.",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_a","output":"[{\"type\":\"output_text\",\"text\":\"a.txt\"}]"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Found a.txt"}]}
		],
		"tools": [{"type":"function","name":"shell","description":"run a shell","parameters":{"type":"object"},"strict":true}],
		"tool_choice": "auto",
		"reasoning": {"effort": "high", "summary": "auto"},
		"store": false,
		"stream": true,
		"include": ["reasoning.encrypted_content"],
		"prompt_cache_key": "abc",
		"parallel_tool_calls": false,
		"client_metadata": {"originator":"codex"}
	}`)
	chat, err := TranslateToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	body := chat.Body
	if body["stream"] != true {
		t.Error("stream should pass through")
	}
	if _, ok := body["store"]; ok {
		t.Error("store must be dropped")
	}
	if _, ok := body["client_metadata"]; ok {
		t.Error("client_metadata must be dropped")
	}
	if _, ok := body["include"]; ok {
		t.Error("include must be dropped")
	}
	if _, ok := body["prompt_cache_key"]; ok {
		t.Error("prompt_cache_key must be dropped")
	}
	if body["parallel_tool_calls"] != false {
		t.Error("parallel_tool_calls should pass through")
	}
	messages := body["messages"].([]map[string]any)
	if messages[0]["role"] != "system" || messages[0]["content"] != "You are a coding agent." {
		t.Errorf("instructions should become leading system message, got %+v", messages[0])
	}
	// user → function_call(assistant tool_calls) → tool result → assistant text
	// （最后的 assistant 文本与 tool_calls assistant 中间隔着 tool 消息，
	// coalesce 只合并相邻 assistant，所以保持独立一条。）
	if len(messages) != 5 {
		t.Fatalf("expected 5 messages, got %d: %+v", len(messages), messages)
	}
	if messages[1]["role"] != "user" {
		t.Errorf("message[1] should be user, got %v", messages[1]["role"])
	}
	if messages[2]["role"] != "assistant" {
		t.Errorf("function_call should become assistant, got %v", messages[2]["role"])
	}
	calls := messages[2]["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("tool call shape wrong: %+v", fn)
	}
	if messages[3]["role"] != "tool" || messages[3]["tool_call_id"] != "call_a" {
		t.Errorf("function_call_output should become tool message, got %+v", messages[3])
	}
	if messages[3]["content"] != "a.txt" {
		t.Errorf("tool output should be unwrapped text, got %q", messages[3]["content"])
	}
	if messages[4]["role"] != "assistant" {
		t.Errorf("trailing assistant text should stay its own message, got %v", messages[4]["role"])
	}
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool should be chat-shaped, got %+v", tool)
	}
	if _, ok := tool["function"]; !ok {
		t.Error("chat tool must nest under function")
	}
	if chat.RequestedEffort != "high" {
		t.Errorf("requested effort should be collected, got %q", chat.RequestedEffort)
	}
}

// 空 text part 与空消息：上游拒绝空 content part，必须剔除。
func TestTranslateDropsEmptyParts(t *testing.T) {
	input := obj(t, `{
		"input": [
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":""},{"type":"output_text","text":"  "}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`)
	chat, _ := TranslateToChat(input)
	messages := chat.Body["messages"].([]map[string]any)
	if len(messages) != 1 {
		t.Fatalf("empty assistant message should be dropped, got %d messages", len(messages))
	}
}

// 孤儿 tool_calls 补合成结果：严格上游要求 tool result 紧跟。
func TestEnsureToolResultsSynthesizes(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "assistant", "tool_calls": []any{map[string]any{
			"id": "call_x", "type": "function",
			"function": map[string]any{"name": "shell", "arguments": "{}"},
		}}},
		{"role": "user", "content": "next turn without tool result"},
	}
	repaired := EnsureToolResultsForCalls(messages)
	if len(repaired) != 4 {
		t.Fatalf("expected synthetic tool result inserted (4 messages), got %d", len(repaired))
	}
	if repaired[1]["role"] != "assistant" {
		t.Errorf("tool_calls assistant stays at [1], got %+v", repaired[1])
	}
	if repaired[2]["role"] != "tool" || repaired[2]["tool_call_id"] != "call_x" {
		t.Errorf("synthetic tool result should follow the tool_calls assistant, got %+v", repaired[2])
	}
	if repaired[2]["content"] != syntheticToolResult {
		t.Errorf("synthetic content mismatch: %v", repaired[2]["content"])
	}
}

// 连续 assistant 消息合并（tool_calls 与文本分离形态）。
func TestCoalesceAssistantMessages(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "go"},
		{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "shell", "arguments": "{}"},
		}}},
		{"role": "assistant", "content": "I ran it."},
		{"role": "user", "content": "thanks"},
	}
	coalesced := CoalesceAssistantMessages(messages)
	if len(coalesced) != 3 {
		t.Fatalf("expected 3 messages after coalesce, got %d", len(coalesced))
	}
	if coalesced[1]["content"] != "I ran it." {
		t.Errorf("text should merge into tool-call assistant, got %v", coalesced[1]["content"])
	}
}

// compaction item 解出 kcr1: base64 摘要并转成 user 消息。
func TestCompactionItemBecomesUserMessage(t *testing.T) {
	input := obj(t, `{
		"input": [
			{"type":"compaction","encrypted_content":"kcr1:RHVtbXkgc3VtbWFyeQ=="},
			{"type":"compaction_trigger"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	chat, _ := TranslateToChat(input)
	messages := chat.Body["messages"].([]map[string]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages (trigger dropped), got %d", len(messages))
	}
	first := messages[0]
	if first["role"] != "user" {
		t.Errorf("compaction should be a user message, got %v", first["role"])
	}
	parts := first["content"].([]any)
	text := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Dummy summary") || !strings.Contains(text, summaryPrefix) {
		t.Errorf("compaction summary text wrong: %q", text)
	}
}

// GLM effort 阶梯：请求档钳到模型声明档。
func TestGLMEffort(t *testing.T) {
	cases := []struct {
		requested string
		levels    []string
		want      string
	}{
		{"low", []string{"low", "high", "max"}, "low"},
		{"medium", []string{"low", "high", "max"}, "low"},
		{"high", []string{"low", "high", "max"}, "high"},
		{"xhigh", []string{"low", "high", "max"}, "max"},
		{"ultra", []string{"low", "high", "max"}, "max"},
		{"low", []string{"high", "max"}, "high"}, // 低于下限落在下限
		{"max", []string{"high", "max"}, "max"},
		{"high", []string{"max"}, "max"},           // 单档模型由外层删参数，钳制函数钳到唯一档
		{"bogus", []string{"low", "high"}, "high"}, // 未知值按 high 对待（Node 版行为）
		{"", []string{"low", "high"}, "high"},      // 缺失同样按 high
	}
	for _, tc := range cases {
		if got := glmEffort(tc.requested, tc.levels); got != tc.want {
			t.Errorf("glmEffort(%q, %v) = %q, want %q", tc.requested, tc.levels, got, tc.want)
		}
	}
}
