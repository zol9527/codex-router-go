package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func agentPayloadItem(encrypted string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nTask: demo\n\nPayload:"},
			map[string]any{"type": "encrypted_content", "encrypted_content": encrypted},
		},
	}
}

// Fernet 判定：gAAAAA 前缀 + base64url 无空白。
func TestFernetDetection(t *testing.T) {
	if !isNativeEncryptedToken("gAAAAABoZWxsbw") {
		t.Error("gAAAAA token must be native")
	}
	if isNativeEncryptedToken("plain text payload") {
		t.Error("plaintext must not be native")
	}
	if isNativeEncryptedToken("gAAAAA has spaces") {
		t.Error("whitespace breaks the Fernet shape")
	}
}

// 形状识别：Message Type + Payload: 结尾才提取；普通消息不碰。
func TestExtractEncryptedAgentPayload(t *testing.T) {
	encrypted, native, ok := extractEncryptedAgentPayload(agentPayloadItem("gAAAAABo"))
	if !ok || encrypted != "gAAAAABo" || !native {
		t.Errorf("extraction wrong: %q %v %v", encrypted, native, ok)
	}
	// 无协作标记的密文 item 不提取。
	plain := map[string]any{
		"type": "message", "role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "just a normal message"},
			map[string]any{"type": "encrypted_content", "encrypted_content": "gAAAAABo"},
		},
	}
	if _, _, ok := extractEncryptedAgentPayload(plain); ok {
		t.Error("non-collaboration item must not extract")
	}
}

// 端到端：native 密文经假 native 端点中继解出，明文进上游请求；
// 第二次请求走缓存（native 端点只被打一次）。
func TestAgentRelayEndToEnd(t *testing.T) {
	relayCalls := 0
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCalls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		// 中继请求形态校验：强制工具调用 + 原样回放的 item。
		tools := body["tools"].([]any)
		tool := tools[0].(map[string]any)
		if tool["name"] != agentPayloadRelayTool {
			t.Errorf("relay must force the relay tool, got %v", tool["name"])
		}
		if body["store"] != false || body["stream"] != true {
			t.Error("relay must be store:false stream:true")
		}
		// 假的 native SSE 响应：function_call 参数里带明文。
		itemID := "fc_1"
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"%s\",\"call_id\":\"call_1\",\"name\":\"%s\",\"arguments\":\"\"}}\n\n", itemID, agentPayloadRelayTool)
		fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"%s\",\"delta\":\"{\\\"payload\\\":\\\"The real task text.\\\"}\"}\n\n", itemID)
		fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"payload\\\":\\\"The real task text.\\\"}\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer native.Close()

	chatUpstreamInput := map[string]any{}
	chatUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		chatUpstreamInput["messages"] = body["messages"]
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer chatUpstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = chatUpstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	srv.opt.NativeBase = native.URL
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("MODEL_ROUTER_AGENT_RELAY_MODEL", "gpt-5.6-sol")
	callerKey, _ := srv.opt.State.CallerKey()

	turn := func() {
		req, _ := http.NewRequest(http.MethodPost,
			ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
			strings.NewReader(`{
				"model": "zai-coding/glm-5.3",
				"input": [{"type":"message","role":"user","content":[
					{"type":"input_text","text":"Message Type: NEW_TASK\nTask: demo\n\nPayload:"},
					{"type":"encrypted_content","encrypted_content":"gAAAAABleGFtcGxlLWNpcGhlcnRleHQtdGhhdC1sb29rcy1uYXRpdmU"}
				]}],
				"stream": true
			}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	turn()
	turn() // 每回合都由 Codex 决定是否重用，Router 不缓存中继结果。

	if relayCalls != 2 {
		t.Errorf("native relay calls = %d, want 2 (no Router cache)", relayCalls)
	}
	messages := chatUpstreamInput["messages"].([]any)
	found := false
	for _, raw := range messages {
		m := raw.(map[string]any)
		content := m["content"]
		if parts, ok := content.([]any); ok {
			for _, partRaw := range parts {
				if part, ok := partRaw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok && strings.Contains(text, "The real task text.") {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Errorf("decrypted plaintext must reach the chat upstream:\n%v", chatUpstreamInput)
	}
}

// 非 Fernet 明文载荷：不经中继直接使用；信封自带 "Payload:" 标签，
// 追加的明文 part 不再重复打标签。
func TestNonFernetPlaintextUsedDirectly(t *testing.T) {
	srv, _ := newTestServer(t)
	input := []any{agentPayloadItem("external-model-wrote-this-plaintext")}
	out := srv.normalizeRoutedAgentInput(context.Background(), input)
	item := out[0].(map[string]any)
	parts := item["content"].([]any)
	last := parts[len(parts)-1].(map[string]any)
	if !strings.Contains(last["text"].(string), "external-model-wrote-this-plaintext") {
		t.Errorf("non-Fernet payload must be used as plaintext: %v", last)
	}
	if strings.Contains(last["text"].(string), "Payload") {
		t.Errorf("appended part must not duplicate the envelope Payload label: %v", last)
	}
}

// 无协作载荷的输入原样返回（零开销）。
func TestNormalizeSkipsPlainInput(t *testing.T) {
	srv, _ := newTestServer(t)
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "hello"},
		}},
	}
	out := srv.normalizeRoutedAgentInput(context.Background(), input)
	if len(out) != 1 {
		t.Fatalf("input must pass through untouched, got %d items", len(out))
	}
}

// 中继响应解析：JSON 形态（非流式兜底）。
func TestParseRelayedPayloadJSON(t *testing.T) {
	body := `{"output":[{"type":"function_call","name":"relay_external_agent_payload","arguments":"{\"payload\":\"json payload text\"}"}]}`
	if got := parseRelayedAgentPayload([]byte(body)); got != "json payload text" {
		t.Errorf("JSON parse = %q", got)
	}
}

// 中继响应解析：SSE done 事件携带完整参数。
func TestParseRelayedPayloadSSEDone(t *testing.T) {
	sse := "data: {\"type\":\"response.function_call_arguments.done\",\"arguments\":\"{\\\"payload\\\":\\\"sse done payload\\\"}\"}\n\ndata: [DONE]\n\n"
	if got := parseRelayedAgentPayload([]byte(sse)); got != "sse done payload" {
		t.Errorf("SSE done parse = %q", got)
	}
}
