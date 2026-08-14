package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// namespace 拍平端到端：上游收到 `<ns>__<tool>` 扁平工具与改名的
// 历史；模型回发扁平调用名，客户端收到 {name, namespace} 形态
// （含 create_thread 的会话模型注入）。
func TestNamespaceFlattenEndToEnd(t *testing.T) {
	var upstreamTools []any
	var upstreamMessages []any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		upstreamTools, _ = body["tools"].([]any)
		upstreamMessages, _ = body["messages"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		// 上游模型回发扁平名调用（模拟 chat 上游的 tool_calls SSE）。
		const delta = `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"codex_app__create_thread","arguments":"{\"title\":\"t\"}"}}]}}]}`
		fmt.Fprintf(w, "data: %s\n\n", delta)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	reqBody := `{
		"model": "zai-coding/glm-5.3",
		"input": [
			{"type":"function_call","name":"navigate","namespace":"codex_app","call_id":"c0","arguments":"{}"},
			{"type":"function_call_output","call_id":"c0","output":"ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn"}]}
		],
		"tools": [
			{"type":"namespace","name":"codex_app","tools":[
				{"name":"create_thread","inputSchema":{"type":"object","properties":{}}},
				{"name":"navigate","inputSchema":{"type":"object"}}
			]},
			{"type":"function","name":"shell","parameters":{"type":"object"}}
		],
		"stream": true
	}`
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	// 上游侧：工具是扁平名。
	flatNames := map[string]bool{}
	for _, raw := range upstreamTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				flatNames[name] = true
			}
		}
	}
	if !flatNames["codex_app__create_thread"] || !flatNames["codex_app__navigate"] || !flatNames["shell"] {
		t.Errorf("flattened tools missing upstream: %v", flatNames)
	}
	// 上游侧：历史 function_call 已改名成扁平形态。
	renamedHistory := false
	for _, raw := range upstreamMessages {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		calls, _ := m["tool_calls"].([]any)
		for _, c := range calls {
			call, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := call["function"].(map[string]any); ok {
				if fn["name"] == "codex_app__navigate" {
					renamedHistory = true
				}
			}
		}
	}
	if !renamedHistory {
		t.Error("history function_call must be renamed to flattened form upstream")
	}

	// 客户端侧：调用还原为 namespace 形态并注入会话模型。
	if !strings.Contains(body, `"name":"create_thread"`) {
		t.Errorf("flattened call must be restored to native name:\n%s", body)
	}
	if !strings.Contains(body, `"namespace":"codex_app"`) {
		t.Errorf("namespace must be restored:\n%s", body)
	}
	if !strings.Contains(body, "zai-coding/glm-5.3") {
		t.Errorf("session model must be injected into create_thread:\n%s", body)
	}
}
