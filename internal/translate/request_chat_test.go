package translate

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
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

// agent_message（Codex 多代理的 NEW_TASK 任务书 / RESULT 回传）必须
// 完整翻译成 user 消息：信封 input_text 与明文 encrypted_content 载荷
// 都要保留。落进 default 占位符分支会让子代理收不到任务
// （2026-08-18 explorer 任务盲事故的根因）。
func TestAgentMessageBecomesUserMessage(t *testing.T) {
	input := obj(t, `{
		"input": [
			{"type":"agent_message","id":"amsg_1","author":"/root/repo_compare","recipient":"/root/repo_compare/explore_opencodex_remote",
			 "content":[
				{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/repo_compare/explore_opencodex_remote\nSender: /root/repo_compare\nPayload:\n"},
				{"type":"encrypted_content","encrypted_content":"任务：核实 https://github.com/lidge-jun/opencodex 的依赖与内存占用。"}
			 ]}
		]
	}`)
	chat, _ := TranslateToChat(input)
	messages := chat.Body["messages"].([]map[string]any)
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0]["role"] != "user" {
		t.Errorf("agent_message should be a user message, got %v", messages[0]["role"])
	}
	text, _ := messages[0]["content"].(string)
	if !strings.Contains(text, "NEW_TASK") || !strings.Contains(text, "核实 https://github.com/lidge-jun/opencodex") {
		t.Errorf("agent_message envelope/payload lost: %q", text)
	}
}

// 回放兜底：历史形态把正文直接放 message 字符串字段；
// 全空载荷丢弃而不是生成占位符。
func TestAgentMessageReplayAndEmptyFallbacks(t *testing.T) {
	chat, _ := TranslateToChat(obj(t, `{
		"input": [
			{"type":"agent_message","message":"我先核对两边的一手证据。"},
			{"type":"agent_message","content":[{"type":"input_text","text":"   "}]}
		]
	}`))
	messages := chat.Body["messages"].([]map[string]any)
	if len(messages) != 1 {
		t.Fatalf("expected 1 message (empty dropped), got %d", len(messages))
	}
	if text, _ := messages[0]["content"].(string); !strings.Contains(text, "一手证据") {
		t.Errorf("replayed agent_message text lost: %q", text)
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

// 思维链携带：reasoning item 的文本落到其后首条 assistant 消息的
// 内部标记上（tool_calls 消息、空 assistant filler 之后的调用都要
// 覆盖）；跨过 user 边界的孤儿 reasoning 丢弃。这是 opencode
// DeepSeek thinking 模式回放契约的翻译期半边（2026-08-15 子代理
// 首轮 400 事故的根因修复）。
func TestReasoningCarryIntoAssistantMessages(t *testing.T) {
	input := obj(t, `{
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Thinking about ls"}]},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_a","output":"done"},
			{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"Second thought"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},
			{"type":"function_call","call_id":"call_b","name":"shell","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_b","output":"ok"},
			{"type":"reasoning","id":"rs_3","summary":[{"type":"summary_text","text":"Orphan"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]},
			{"type":"reasoning","id":"rs_4","content":[{"type":"output_text","text":"Content-array form"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Answer"}]}
		]
	}`)
	chat, err := TranslateToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	messages := chat.Body["messages"].([]map[string]any)
	// user, assistant(call_a)+carry, tool, assistant(call_b)+carry, tool,
	// user, assistant("Answer")+carry(content 数组形态)。空 assistant
	// filler 在翻译期被丢弃，carry 必须落到后面的 call_b 上。
	if len(messages) != 7 {
		t.Fatalf("expected 7 messages, got %d: %+v", len(messages), messages)
	}
	if carry := messages[1][reasoningCarryKey]; carry != "Thinking about ls" {
		t.Errorf("call_a assistant should carry reasoning, got %v", carry)
	}
	if carry := messages[3][reasoningCarryKey]; carry != "Second thought" {
		t.Errorf("call_b assistant should carry reasoning past empty filler, got %v", carry)
	}
	if carry := messages[6][reasoningCarryKey]; carry != "Content-array form" {
		t.Errorf("content-array reasoning should carry, got %v", carry)
	}
	for i, message := range messages {
		if i == 1 || i == 3 || i == 6 {
			continue
		}
		if _, ok := message[reasoningCarryKey]; ok {
			t.Errorf("message %d must not carry reasoning", i)
		}
	}
}

// carry 必须在 CoalesceAssistantMessages 的合并中存活：reasoning 后的
// assistant 文本与紧随的 function_call 合并成一条时，标记留在合并头。
func TestReasoningCarrySurvivesCoalesce(t *testing.T) {
	input := obj(t, `{
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Plan"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running it"}]},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"}
		]
	}`)
	chat, _ := TranslateToChat(input)
	messages := chat.Body["messages"].([]map[string]any)
	// user + 合并 assistant + 孤儿 call 的合成 tool 结果。
	if len(messages) != 3 {
		t.Fatalf("expected user + coalesced assistant + synthetic tool, got %d: %+v", len(messages), messages)
	}
	merged := messages[1]
	if carry := merged[reasoningCarryKey]; carry != "Plan" {
		t.Errorf("coalesced assistant must keep carry, got %v", carry)
	}
	if !strings.Contains(fmt.Sprint(merged["content"]), "Running it") {
		t.Errorf("coalesced content wrong: %v", merged["content"])
	}
	if _, ok := merged["tool_calls"]; !ok {
		t.Error("coalesced assistant must keep tool_calls")
	}
}

// deepseek-thinking 画像：标记提升为 reasoning_content；强制
// tool_choice 降级 auto（thinking 模式拒绝 "required"/function 对象）。
func TestDeepSeekThinkingProfilePromotesCarry(t *testing.T) {
	input := obj(t, `{
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Prior thought"}]},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"}
		],
		"tool_choice": "required"
	}`)
	chat, _ := TranslateToChat(input)
	model := &registry.Model{RequestProfile: "deepseek-thinking"}
	ApplyRequestProfile(chat.Body, "high", model)
	messages := chat.Body["messages"].([]map[string]any)
	if messages[1]["reasoning_content"] != "Prior thought" {
		t.Errorf("carry must be promoted to reasoning_content, got %v", messages[1]["reasoning_content"])
	}
	if _, ok := messages[1][reasoningCarryKey]; ok {
		t.Error("marker must be removed after promotion")
	}
	if chat.Body["tool_choice"] != "auto" {
		t.Errorf("forced tool_choice must downgrade to auto, got %v", chat.Body["tool_choice"])
	}
	if _, ok := chat.Body["thinking"]; ok {
		t.Error("opencode relay variant must not send a thinking object")
	}
}

// 其余画像（默认 / glm-thinking）必须剥除内部标记：官方 DeepSeek API
// 对输入里的 reasoning_content 报 400，标记字段也绝不外发。
func TestOtherProfilesStripCarry(t *testing.T) {
	input := obj(t, `{
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Prior thought"}]},
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{}"}
		]
	}`)
	for _, profile := range []string{"", "glm-thinking"} {
		chat, _ := TranslateToChat(input)
		ApplyRequestProfile(chat.Body, "high", &registry.Model{RequestProfile: profile})
		messages := chat.Body["messages"].([]map[string]any)
		if _, ok := messages[1][reasoningCarryKey]; ok {
			t.Errorf("profile %q must strip the carry marker", profile)
		}
		if _, ok := messages[1]["reasoning_content"]; ok {
			t.Errorf("profile %q must not emit reasoning_content", profile)
		}
	}
}

// custom 工具往返：声明翻译成 {input: string} function + 名字收集；
// 响应侧对这些名字的调用还原成 custom_tool_call（自由文本 input）。
// 背景（2026-08-15 事故）：catalog 继承了原生模板的
// tool_mode=code_mode_only，Codex 以 custom 形态下发 code-mode exec，
// 声明被静默丢弃后模型自造 {"cmd"/"command"} 载荷，Codex 报
// "tool exec invoked with incompatible payload"。
func TestCustomToolDeclarationTranslated(t *testing.T) {
	input := obj(t, `{
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}],
		"tools": [
			{"type":"custom","name":"exec","description":"Run code.","format":{"type":"text"}},
			{"type":"function","name":"shell","description":"run a shell","parameters":{"type":"object"}}
		]
	}`)
	chat, err := TranslateToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.CustomTools) != 1 || chat.CustomTools[0] != "exec" {
		t.Fatalf("custom tool names wrong: %v", chat.CustomTools)
	}
	tools := chat.Body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("both tools must be declared, got %d", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "exec" {
		t.Fatalf("custom tool must keep its name, got %v", fn["name"])
	}
	params := fn["parameters"].(map[string]any)
	if params["required"].([]any)[0] != "input" {
		t.Errorf("custom tool schema must require input, got %v", params["required"])
	}
}

// custom 工具的调用在 SSE 收尾时必须是 custom_tool_call item，
// input 从伪装 schema 的 {"input": ...} 解出；done 事件携带完整载荷
// （Codex 的解析器从 output_item.done 取 custom 调用）。
func TestCustomToolCallRoundTripStream(t *testing.T) {
	translator := NewChatToResponsesSSE("", "deepseek").WithCustomTools([]string{"exec"})
	// arguments 是嵌套 JSON 字符串，用 strconv.Quote 构造避免手写转义出错。
	chunk := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"exec","arguments":` +
		strconv.Quote(`{"input":"const x = 1;"}`) + `}}]}}]}`
	var collected []byte
	collected = append(collected, translator.Created()...)
	collected = append(collected, translator.Feed(chunk)...)
	collected = append(collected, translator.Feed("[DONE]")...)
	text := string(collected)
	if !strings.Contains(text, `"type":"custom_tool_call"`) {
		t.Errorf("custom tool call must emit custom_tool_call items:\n%s", text)
	}
	if !strings.Contains(text, "const x = 1;") {
		t.Errorf("custom_tool_call input must carry the payload text:\n%s", text)
	}
	if strings.Contains(text, `"type":"function_call"`) {
		t.Errorf("custom tool call must not emit function_call items:\n%s", text)
	}
}

// 模型没按伪装 schema 输出时（裸 JSON 字符串 / 乱形状），载荷退化到
// 原始 arguments，调用不丢。
func TestCustomToolInputFallbacks(t *testing.T) {
	if got := customToolInput(`"raw string payload"`); got != "raw string payload" {
		t.Errorf("bare JSON string should be unwrapped, got %q", got)
	}
	if got := customToolInput(`{"cmd":"ls"}`); got != `{"cmd":"ls"}` {
		t.Errorf("unknown shape should fall back to raw arguments, got %q", got)
	}
}

// 空载荷 custom 调用不算内容（2026-08-17 GLM-5.3 大上下文退化事故：
// 模型反复发空参数 exec，Codex needs_follow_up 无限续轮 200+ 次）。
// arguments 缺失、字面 "{}"、"{"input":""}" 三种形态都必须判空。
func TestEmptyCustomToolCallHasNoContent(t *testing.T) {
	cases := map[string]string{
		"missing":     `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_e","function":{"name":"exec"}}]}}]}`,
		"empty-obj":   `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_e","function":{"name":"exec","arguments":"{}"}}]}}]}`,
		"empty-input": `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_e","function":{"name":"exec","arguments":"{\"input\":\"\"}"}}]}}]}`,
		"blank-input": `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_e","function":{"name":"exec","arguments":"{\"input\":\"  \"}"}}]}}]}`,
		"whitespace":  `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_e","function":{"name":"exec","arguments":"   "}}]}}]}`,
	}
	for name, chunk := range cases {
		translator := NewChatToResponsesSSE("", "glm").WithCustomTools([]string{"exec"})
		translator.Created()
		translator.Feed(chunk)
		translator.Feed(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		translator.Feed("[DONE]")
		if translator.HasContent() {
			t.Errorf("%s: 空载荷 custom 调用不算内容", name)
		}
	}

	// 对照一：非空载荷的 custom 调用照旧算内容。
	withPayload := NewChatToResponsesSSE("", "glm").WithCustomTools([]string{"exec"})
	withPayload.Created()
	withPayload.Feed(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_f","function":{"name":"exec","arguments":"{\"input\":\"ls\"}"}}]}}]}`)
	withPayload.Feed("[DONE]")
	if !withPayload.HasContent() {
		t.Error("带载荷的 custom 调用必须算内容")
	}

	// 对照二：普通 function 调用参数为空是合法无参形态，照旧算内容。
	plain := NewChatToResponsesSSE("", "glm")
	plain.Created()
	plain.Feed(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_g","function":{"name":"shell","arguments":""}}]}}]}`)
	plain.Feed("[DONE]")
	if !plain.HasContent() {
		t.Error("无参 function 调用是合法形态，必须算内容")
	}
}

// 非流式路径：custom 调用出现在 completed 的 output 数组里。
func TestCustomToolCallNonStream(t *testing.T) {
	body := obj(t, `{
		"choices": [{"message":{"role":"assistant","tool_calls":[
			{"id":"call_y","type":"function","function":{"name":"exec","arguments":"{\"input\":\"await tools.read('/x')\"}"}}
		]}}]
	}`)
	translator := NewChatToResponsesSSE("", "deepseek").WithCustomTools([]string{"exec"})
	response := TranslateNonStreamChatWith(body, translator)
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected one output item, got %d", len(output))
	}
	item := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("non-stream custom call must be custom_tool_call, got %v", item["type"])
	}
	if item["input"] != "await tools.read('/x')" {
		t.Errorf("custom_tool_call input wrong: %v", item["input"])
	}
	if item["call_id"] != "call_y" {
		t.Errorf("custom_tool_call call_id must survive, got %v", item["call_id"])
	}
}
