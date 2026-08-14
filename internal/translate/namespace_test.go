package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func nsToolsFixture() []any {
	return []any{
		map[string]any{
			"type": "namespace", "name": "codex_app",
			"tools": []any{
				map[string]any{"name": "create_thread", "description": "new thread",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}},
				map[string]any{"name": "navigate", "inputSchema": map[string]any{"type": "object"}},
			},
		},
		map[string]any{
			"type": "namespace", "name": "collaboration",
			"tools": []any{
				map[string]any{
					"name": "spawn_agent",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"model": map[string]any{
								"anyOf": []any{
									map[string]any{"const": "zai-coding/glm-5.3"},
									map[string]any{"enum": []any{"gpt-5.5", "zai-coding/glm-5.2"}},
								},
							},
						},
					},
				},
			},
		},
		map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "tool_search", "query": "find tools"},
	}
}

// 拍平：namespace → `<ns>__<tool>` 扁平函数，inputSchema 别名 parameters，
// tool_search 丢弃，namespace 名含分隔符也正确保留。
func TestFlattenNamespaceTools(t *testing.T) {
	result := FlattenNamespaceTools(nsToolsFixture())
	if !result.Flattened {
		t.Fatal("should flatten")
	}
	names := map[string]bool{}
	for _, raw := range result.Tools.([]any) {
		tool := raw.(map[string]any)
		if tool["type"] == "tool_search" {
			t.Error("tool_search must be dropped")
		}
		if name, ok := tool["name"].(string); ok {
			names[name] = true
		}
		// 每个工具都应有 parameters（inputSchema 别名）。
		if tool["type"] == "function" {
			if tool["parameters"] == nil && tool["inputSchema"] != nil {
				t.Errorf("flattened tool %v missing parameters alias", tool["name"])
			}
		}
	}
	for _, want := range []string{
		"codex_app__create_thread", "codex_app__navigate",
		"collaboration__spawn_agent", "shell",
	} {
		if !names[want] {
			t.Errorf("missing flattened name %q; got %v", want, names)
		}
	}
	if names["spawn_agent"] {
		t.Error("bare namespace child must not survive")
	}
	// spawn_agent 模型枚举被收集。
	index := result.Index()
	for _, want := range []string{"zai-coding/glm-5.3", "gpt-5.5", "zai-coding/glm-5.2"} {
		if !index.spawnAgentModels[want] {
			t.Errorf("spawn agent model %q not collected", want)
		}
	}
}

// 历史改名：三种形态（已扁平不动 / {name,namespace} 精确 / 裸名唯一归属）。
func TestFlattenNamespacedHistory(t *testing.T) {
	result := FlattenNamespaceTools(nsToolsFixture())
	history := []any{
		map[string]any{"type": "function_call", "name": "codex_app__navigate", "call_id": "1"},
		map[string]any{"type": "function_call", "name": "navigate", "namespace": "codex_app", "call_id": "2"},
		map[string]any{"type": "function_call", "name": "navigate", "call_id": "3"}, // 裸名唯一归属 codex_app
		map[string]any{"type": "function_call", "name": "shell", "call_id": "4"},    // 非 namespace 工具不动
		map[string]any{"type": "message", "role": "user"},                           // 非 function_call 不动
	}
	out := FlattenNamespacedHistory(history, result.Namespaces)
	var names []string
	for _, raw := range out {
		item := raw.(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		if _, has := item["namespace"]; has {
			t.Errorf("namespace field must be stripped after rename: %v", item)
		}
		names = append(names, item["name"].(string))
	}
	want := []string{"codex_app__navigate", "codex_app__navigate", "codex_app__navigate", "shell"}
	for i := 0; i < 4; i++ {
		if names[i] != want[i] {
			t.Errorf("history[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// 还原：模型发出扁平名 → 客户端 {name, namespace}；
// spawn_agent 带白名单外的 model 参数 → 删除；
// create_thread 无 model → 注入会话模型；
// 整数 token 20000.0 → 20000。
func TestRewriteFunctionCallRestoresNamespace(t *testing.T) {
	result := FlattenNamespaceTools(nsToolsFixture())
	index := result.Index()

	// 1. 扁平名精确还原。
	item := map[string]any{
		"type": "function_call", "name": "codex_app__navigate",
		"call_id": "c1", "arguments": `{}`,
	}
	rewritten := index.RewriteFunctionCallItem(item, "")
	if rewritten == nil {
		t.Fatal("flat name should restore")
	}
	if rewritten["name"] != "navigate" || rewritten["namespace"] != "codex_app" {
		t.Errorf("restore wrong: %v / %v", rewritten["name"], rewritten["namespace"])
	}

	// 2. spawn_agent 白名单外的 model 删除。
	spawn := map[string]any{
		"type": "function_call", "name": "collaboration__spawn_agent",
		"call_id": "c2", "arguments": `{"prompt":"x","model":"made-up/model"}`,
	}
	rewritten = index.RewriteFunctionCallItem(spawn, "")
	if rewritten == nil {
		t.Fatal("spawn_agent should be rewritten")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rewritten["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if _, has := args["model"]; has {
		t.Errorf("invalid model must be dropped: %v", args)
	}
	if args["prompt"] != "x" {
		t.Error("other arguments must survive")
	}

	// 3. 白名单内的 model 保留。
	valid := map[string]any{
		"type": "function_call", "name": "collaboration__spawn_agent",
		"call_id": "c3", "arguments": `{"model":"gpt-5.5"}`,
	}
	if rewritten = index.RewriteFunctionCallItem(valid, ""); rewritten == nil {
		t.Fatal("valid spawn_agent rewrites (namespace restore)")
	}
	if !strings.Contains(rewritten["arguments"].(string), "gpt-5.5") {
		t.Errorf("allowed model must survive: %v", rewritten["arguments"])
	}

	// 4. create_thread 注入会话模型（云端 target 除外）。
	thread := map[string]any{
		"type": "function_call", "name": "codex_app__create_thread",
		"call_id": "c4", "arguments": `{"title":"t"}`,
	}
	rewritten = index.RewriteFunctionCallItem(thread, "zai-coding/glm-5.3")
	if rewritten == nil {
		t.Fatal("create_thread should inject session model")
	}
	if !strings.Contains(rewritten["arguments"].(string), "zai-coding/glm-5.3") {
		t.Errorf("session model must be injected: %v", rewritten["arguments"])
	}
	cloud := map[string]any{
		"type": "function_call", "name": "codex_app__create_thread",
		"call_id": "c5", "arguments": `{"target":{"type":"chatgptWorkCloud"}}`,
	}
	if rewritten = index.RewriteFunctionCallItem(cloud, "zai-coding/glm-5.3"); rewritten != nil {
		// namespace 还原本身是必要改写；但 model 不得被注入。
		if strings.Contains(rewritten["arguments"].(string), "zai-coding/glm-5.3") {
			t.Error("cloud target must not get session model injected")
		}
	}

	// 5. 整数 token 修复。
	fixed := map[string]any{
		"type": "function_call", "name": "codex_app__navigate",
		"call_id": "c6", "arguments": `{"line":20000.0,"name":"a"}`,
	}
	rewritten = index.RewriteFunctionCallItem(fixed, "")
	if rewritten == nil || !strings.Contains(rewritten["arguments"].(string), `"line":20000,`) {
		t.Errorf("whole-number float must be fixed: %v", rewritten)
	}
}

// 端到端：翻译器事件流里的 function_call 被还原为 namespace 形态。
func TestTranslatorRestoresNamespaceInEvents(t *testing.T) {
	result := FlattenNamespaceTools(nsToolsFixture())
	index := result.Index()
	tr := NewChatToResponsesSSE("", "m").WithNamespaceIndex(index, "zai-coding/glm-5.3")
	out := tr.Feed(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"codex_app__create_thread","arguments":"{\"title\":\"go\"}"}}]}}]}`)
	close := tr.Feed(`[DONE]`)
	all := string(out) + string(close)
	if !strings.Contains(all, `"name":"create_thread"`) {
		t.Errorf("flattened name must be restored in events:\n%s", all)
	}
	if !strings.Contains(all, `"namespace":"codex_app"`) {
		t.Errorf("namespace must be present in events:\n%s", all)
	}
	if !strings.Contains(all, "zai-coding/glm-5.3") {
		t.Errorf("session model must be injected into create_thread:\n%s", all)
	}
}

// 裸名冲突不还原（两个 namespace 同名工具）。
func TestBareNameCollisionNotRestored(t *testing.T) {
	tools := []any{
		map[string]any{"type": "namespace", "name": "ns_a", "tools": []any{
			map[string]any{"name": "run", "inputSchema": map[string]any{"type": "object"}},
		}},
		map[string]any{"type": "namespace", "name": "ns_b", "tools": []any{
			map[string]any{"name": "run", "inputSchema": map[string]any{"type": "object"}},
		}},
	}
	result := FlattenNamespaceTools(tools)
	index := result.Index()
	item := map[string]any{
		"type": "function_call", "name": "run", "call_id": "c", "arguments": "{}",
	}
	if rewritten := index.RewriteFunctionCallItem(item, ""); rewritten != nil {
		t.Errorf("ambiguous bare name must stay untouched, got %v", rewritten)
	}
	// 但扁平名精确还原不受冲突影响。
	flat := map[string]any{
		"type": "function_call", "name": "ns_a__run", "call_id": "c", "arguments": "{}",
	}
	if rewritten := index.RewriteFunctionCallItem(flat, ""); rewritten == nil {
		t.Error("exact flat name must resolve")
	} else if rewritten["namespace"] != "ns_a" {
		t.Errorf("resolved to wrong namespace: %v", rewritten["namespace"])
	}
}

// ---- schema 归一 ----

// 矛盾 literal：[string] enum 里的 true 被丢弃，合法值保留。
func TestSchemaLiteralNormalization(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"lane": map[string]any{"type": "string", "enum": []any{"fast", true, "slow"}},
		},
	}
	normalized := ProviderToolSchema(schema)
	props := normalized.(map[string]any)["properties"].(map[string]any)
	lane := props["lane"].(map[string]any)
	enum := lane["enum"].([]any)
	if len(enum) != 2 || enum[0] != "fast" || enum[1] != "slow" {
		t.Errorf("contradicting literal must be dropped: %v", enum)
	}
}

// union 根合并：oneOf 分支的 properties 合并，required 取交集。
func TestUnionRootMerge(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"oneOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mode":  map[string]any{"type": "string", "const": "view"},
					"width": map[string]any{"type": "integer"},
				},
				"required": []any{"mode", "width"},
			},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mode": map[string]any{"type": "string", "const": "delete"},
					"id":   map[string]any{"type": "string"},
				},
				"required": []any{"mode", "id"},
			},
		},
		"required": []any{"shared"},
	}
	merged := ProviderToolSchema(schema).(map[string]any)
	if merged["type"] != "object" {
		t.Errorf("merged root must be object: %v", merged["type"])
	}
	props := merged["properties"].(map[string]any)
	for _, want := range []string{"mode", "width", "id"} {
		if props[want] == nil {
			t.Errorf("merged property %q missing", want)
		}
	}
	required := merged["required"].([]string)
	// 交集规则：mode 被两个分支共同要求而保留；width/id 分支分歧而省略。
	if len(required) != 2 {
		t.Errorf("required = root shared + branch-intersection mode: %v", required)
	}
	hasShared, hasMode := false, false
	for _, name := range required {
		if name == "shared" {
			hasShared = true
		}
		if name == "mode" {
			hasMode = true
		}
	}
	if !hasShared || !hasMode {
		t.Errorf("required must be [shared mode]: %v", required)
	}
	if merged["additionalProperties"] != true {
		t.Error("merged schema must accept branch-discriminating fields")
	}
}

// 整数 token 修复的边界。
func TestCoerceFunctionCallArguments(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":20000.0}`, `{"a":20000}`},
		{`{"a":2e4}`, `{"a":20000}`},
		{`{"a":1.5}`, `{"a":1.5}`},                   // 真小数不动
		{`{"a":"20000.0"}`, `{"a":"20000.0"}`},       // 字符串内不动
		{`{"a":-0}`, `{"a":0}`},                      // -0 → 0
		{`{"a":3.14,"b":42.0}`, `{"a":3.14,"b":42}`}, // 混合
	}
	for _, tc := range cases {
		if got := CoerceFunctionCallArguments(tc.in); got != tc.want {
			t.Errorf("Coerce(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
