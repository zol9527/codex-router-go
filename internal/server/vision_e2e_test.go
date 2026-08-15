package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/vision"
)

// 端到端：native 会话请求带图片 → native 引擎读图（假端点）→
// 转录文本替换进 chat 上游收到的请求。
func TestVisionBridgeEndToEnd(t *testing.T) {
	nativeDescribe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("native describe path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer codex-session" {
			t.Errorf("native describe must carry the caller's session")
		}
		// 后端强制流式（非流式 400 "Stream must be set to true"）。
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.output_text.delta\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"## Summary\\nA red error dialog reading `boom`.\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer nativeDescribe.Close()

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
	srv.opt.NativeBase = nativeDescribe.URL
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	// merged 目录放进一个 listed 的视觉原生模型（native 引擎候选）。
	merged := map[string]any{"models": []any{
		map[string]any{"slug": "gpt-5.6-sol", "display_name": "GPT-5.6-Sol",
			"visibility": "list", "input_modalities": "text, image", "priority": 5},
	}}
	raw, _ := json.Marshal(merged)
	writeStateFile(t, srv, "merged-models.json", string(raw))

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{
			"model": "zai-coding/glm-5.3",
			"input": [{"type":"message","role":"user","content":[
				{"type":"input_text","text":"<image path=\"/tmp/err.png\">"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAB"},
				{"type":"input_text","text":"what does this say?"}
			]}],
			"stream": true
		}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer codex-session")
	req.Header.Set("Chatgpt-Account-Id", "acct-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// chat 上游收到的 user 消息不再含图片，含证据文本。
	messages := chatUpstreamInput["messages"].([]any)
	foundEvidence := false
	for _, raw := range messages {
		m := raw.(map[string]any)
		if m["role"] != "user" {
			continue
		}
		if parts, ok := m["content"].([]any); ok {
			for _, partRaw := range parts {
				if part, ok := partRaw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok &&
						strings.Contains(text, "A red error dialog") &&
						strings.Contains(strings.ToLower(text), "gpt-5.6-sol") {
						foundEvidence = true
					}
				}
			}
		}
	}
	if !foundEvidence {
		t.Errorf("evidence must replace the image upstream:\n%v", chatUpstreamInput)
	}
}

// 桥关闭（enabled=false）时图片原样到达上游 —— 翻译器把 input_image
// 转成 image_url part（vision.StripImages 未启用，chat 形态透传）。
func TestVisionBridgeDisabledPassthrough(t *testing.T) {
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
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()
	writeStateFile(t, srv, "vision-bridge.json", `{"enabled": false}`)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{
			"model": "zai-coding/glm-5.3",
			"input": [{"type":"message","role":"user","content":[
				{"type":"input_image","image_url":"data:image/png;base64,AAAB"}
			]}],
			"stream": true
		}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// 关闭时零干预：请求正常完成（上游收到 image_url part，由 provider 处置）。
	if resp.StatusCode != http.StatusOK {
		t.Errorf("disabled bridge must not fail the turn: %d", resp.StatusCode)
	}
	if !vision.Configured(srv.opt.State.Dir) {
		t.Error("state file should be configured")
	}
}

func writeStateFile(t *testing.T, srv *Server, name, content string) {
	t.Helper()
	if err := os.WriteFile(srv.opt.State.Dir+"/"+name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// anthropic 协议引擎端到端：pin 的 messages 变体引擎读图走 /messages
// （x-api-key + image source 块），证据文本替换进 chat 上游 ——
// 2026-08-16 前 qwen3.7-max 作为 pin 每次结构性失败，只能靠原生
// 引擎回退。
func TestVisionBridgeAnthropicEngine(t *testing.T) {
	var anthropicCalls int
	anthropicInput := map[string]any{}
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls++
		if r.URL.Path != "/messages" {
			t.Errorf("anthropic describe path = %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-opencode-key" {
			t.Errorf("anthropic describe must send x-api-key, got %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic describe must send anthropic-version")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		anthropicInput["body"] = body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"## Summary\nA qwen dialog via the anthropic path."}]}`)
	}))
	defer anthropicUpstream.Close()

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
	t.Setenv("ZAI_API_KEY", "")
	// 注入 anthropic 协议引擎（messages 变体）+ 对应 provider。
	srv.opt.Registry.Providers["opencode-go-messages"] = &registry.Provider{
		ID: "opencode-go-messages", DisplayName: "opencode Go Messages", Kind: "openai-compatible",
		OwnedBy: "opencode", VariantOf: "opencode-go", Protocol: "anthropic",
		BaseURL: anthropicUpstream.URL, BaseURLEnv: "",
		Credential: registry.Credential{File: "opencode-go-api-key.secret"},
	}
	srv.opt.Registry.Models = append(srv.opt.Registry.Models, &registry.Model{
		Slug: "opencode-go-messages/qwen3.7-max", GatewayModel: "opencode-go-messages-qwen3-7-max",
		UpstreamModel: "qwen3.7-max", Provider: "opencode-go-messages", Listed: true,
		DisplayName: "Qwen3.7 Max (opencode Go)", Priority: 43,
		InputModalities: []string{"text", "image"},
	})
	writeStateFile(t, srv, "vision-bridge.json", `{"enabled": true, "engine": "opencode-go-messages/qwen3.7-max"}`)
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{
			"model": "zai-coding/glm-5.3",
			"input": [{"type":"message","role":"user","content":[
				{"type":"input_text","text":"what does this say?"},
				{"type":"input_image","image_url":"data:image/png;base64,AAAB"}
			]}],
			"stream": true
		}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer codex-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if anthropicCalls == 0 {
		t.Fatal("pinned anthropic engine must be consulted")
	}
	body := anthropicInput["body"].(map[string]any)
	if body["model"] != "qwen3.7-max" {
		t.Errorf("anthropic model must be upstream id, got %v", body["model"])
	}
	blocks := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	image := blocks[1].(map[string]any)
	source := image["source"].(map[string]any)
	if source["media_type"] != "image/png" || source["data"] != "AAAB" {
		t.Errorf("anthropic image source wrong: %v", source)
	}
	messages := chatUpstreamInput["messages"].([]any)
	foundEvidence := false
	for _, raw := range messages {
		if m, ok := raw.(map[string]any); ok && m["role"] == "user" {
			if parts, ok := m["content"].([]any); ok {
				for _, partRaw := range parts {
					if part, ok := partRaw.(map[string]any); ok {
						if text, ok := part["text"].(string); ok && strings.Contains(text, "qwen dialog via the anthropic path") {
							foundEvidence = true
						}
					}
				}
			}
		}
	}
	if !foundEvidence {
		t.Errorf("anthropic evidence must replace the image upstream:\n%v", chatUpstreamInput)
	}
}

// 上游 SSE 以 EOF 结束但不发 [DONE] 哨兵（zai 偶发）：路由器必须
// 补齐 response.completed —— 否则 Codex 判 "stream closed before
// response.completed" 整轮重试（2026-08-16 01:10-01:12 实发三连重试，
// 思考型模型 60-90s/轮）。
func TestStreamEOFWithoutDoneSentinelCompletes(t *testing.T) {
	chatUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 一个内容增量后直接断流（无 [DONE]）。
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer chatUpstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = chatUpstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{
			"model": "zai-coding/glm-5.3",
			"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
			"stream": true
		}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "answer") {
		t.Errorf("content delta must survive:\n%s", text)
	}
	if !strings.Contains(text, "\"type\":\"response.completed\"") {
		t.Errorf("EOF without [DONE] must still complete the response:\n%s", text)
	}
	if !strings.Contains(text, "data: [DONE]") {
		t.Errorf("terminal [DONE] must be synthesized:\n%s", text)
	}
}
