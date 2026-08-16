package translate

import (
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
)

// 直接测试：配对 tool 行获得命令头，非配对/异常形态不受影响。
func TestMirrorToolCallArgumentsDirect(t *testing.T) {
	body := map[string]any{"messages": []map[string]any{
		{"role": "user", "content": "go"},
		{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
				"name": "exec", "arguments": `"const patch = 'Add File: a.md'"`}},
			map[string]any{"id": "call_2", "type": "function", "function": map[string]any{
				"name": "exec", "arguments": "{}"}},
		}},
		{"role": "tool", "tool_call_id": "call_1", "content": "done"},
		{"role": "tool", "tool_call_id": "call_2", "content": ""},
		{"role": "tool", "tool_call_id": "call_unknown", "content": "orphan"},
		{"role": "assistant", "content": "fin"},
	}}

	MirrorToolCallArguments(body)
	messages := body["messages"].([]map[string]any)

	want1 := "Command (exec):\n\"const patch = 'Add File: a.md'\"\nResult:\ndone"
	if got := messages[2]["content"]; got != want1 {
		t.Errorf("call_1 tool content = %q, want %q", got, want1)
	}
	// 空参数 "{}"（Codex 空探测调用）只保留命令名行；空结果保留头骨架。
	want2 := "Command (exec).\nResult:\n"
	if got := messages[3]["content"]; got != want2 {
		t.Errorf("call_2 tool content = %q, want %q", got, want2)
	}
	// id 对不上任何 call 的 tool 行原样保留。
	if got := messages[4]["content"]; got != "orphan" {
		t.Errorf("orphan tool content = %q, want untouched", got)
	}
}

// 确定性：同一输入镜像两次产物逐字节相同（前缀缓存契约）。
func TestMirrorToolCallArgumentsDeterministic(t *testing.T) {
	build := func() map[string]any {
		return map[string]any{"messages": []map[string]any{
			{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c", "type": "function", "function": map[string]any{
					"name": "shell", "arguments": `{"cmd":"ls -la"}`}}},
			},
			{"role": "tool", "tool_call_id": "c", "content": "total 0"},
		}}
	}
	a, b := build(), build()
	MirrorToolCallArguments(a)
	MirrorToolCallArguments(b)
	am := a["messages"].([]map[string]any)
	bm := b["messages"].([]map[string]any)
	if am[1]["content"] != bm[1]["content"] {
		t.Errorf("mirror not deterministic: %q vs %q", am[1]["content"], bm[1]["content"])
	}
}

// 事故复现形态：glm-thinking profile 下，custom_tool_call（apply_patch 载荷）
// 的参数必须出现在配对 tool 结果里——Z.ai 丢 arguments 的补偿路径。
func TestGLMThinkingProfileMirrorsArguments(t *testing.T) {
	input := map[string]any{
		"model":        "zai-coding/glm-5.3",
		"instructions": "You are Codex.",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "write docs"}}},
			map[string]any{"type": "custom_tool_call", "call_id": "call_p", "name": "exec",
				"input": "const patch = `*** Begin Patch\n*** Add File: docs/architecture/a.md\n+# Title\n*** End Patch`;"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_p",
				"output": []any{map[string]any{"type": "input_text", "text": "Script completed\nOutput:\n{}"}}},
		},
		"tools":  []any{map[string]any{"type": "custom", "name": "exec", "description": "run"}},
		"stream": true,
	}
	chat, err := TranslateToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	model := &registry.Model{RequestProfile: "glm-thinking", ReasoningLevels: []registry.ReasoningLevel{{Effort: "high"}, {Effort: "max"}}}
	ApplyRequestProfile(chat.Body, "max", model)

	messages := chat.Body["messages"].([]map[string]any)
	var toolContent string
	for _, m := range messages {
		if m["role"] == "tool" && m["tool_call_id"] == "call_p" {
			toolContent, _ = m["content"].(string)
		}
	}
	if toolContent == "" {
		t.Fatal("tool result for call_p not found")
	}
	if !strings.Contains(toolContent, "Command (exec):") {
		t.Errorf("missing command header in %q", toolContent)
	}
	if !strings.Contains(toolContent, "Add File: docs/architecture/a.md") {
		t.Errorf("patch payload not visible in %q", toolContent)
	}
	if !strings.Contains(toolContent, "Script completed") {
		t.Errorf("original output lost in %q", toolContent)
	}
}

// 默认 profile（opencode-go 等 arguments 处理正常的上游）不镜像。
func TestOtherProfilesDoNotMirror(t *testing.T) {
	input := obj(t, `{
		"model": "opencode-go/deepseek-v4-flash",
		"input": [
			{"type":"function_call","call_id":"call_a","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_a","output":"a.txt"}
		],
		"tools": [{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"stream": false
	}`)
	chat, err := TranslateToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	ApplyRequestProfile(chat.Body, "high", &registry.Model{})
	messages := chat.Body["messages"].([]map[string]any)
	for _, m := range messages {
		if m["role"] == "tool" {
			if got, _ := m["content"].(string); got != "a.txt" {
				t.Errorf("default profile must not mirror, got %q", got)
			}
		}
	}
}
