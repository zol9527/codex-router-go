package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/domain/cred"
	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/state"
)

// newTestServer 装配一个指向测试 state/config 的 Server。
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	stateDir := t.TempDir()
	st, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureSecrets(); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabledProviders([]string{"zai-coding", "opencode-go", "opencode-go-responses"}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteCredentialFile("zai-coding-api-key.secret", "test-zai-key"); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteCredentialFile("opencode-go-api-key.secret", "test-opencode-key"); err != nil {
		t.Fatal(err)
	}
	// 测试内联注册表：不依赖仓库 config/ 目录（单测自包含）。
	reg := inlineRegistry()
	srv, err := New(Options{
		State:       st,
		Registry:    reg,
		Credentials: cred.New(st),
		ListenAddr:  "127.0.0.1:0",
		NativeBase:  "http://native.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// inlineRegistry 构建与真实 config/ 同构的三 provider 测试注册表。
func inlineRegistry() *registry.Registry {
	return registry.FromDefinitions(
		[]registry.Provider{
			{ID: "zai-coding", DisplayName: "Z.ai GLM Coding Plan", Kind: "openai-compatible",
				OwnedBy: "zai", BaseURL: "https://api.z.ai/api/coding/paas/v4", BaseURLEnv: "ZAI_CODING_BASE_URL",
				Credential: registry.Credential{Environment: []string{"ZAI_API_KEY"},
					File: "zai-coding-api-key.secret", KeychainServices: []string{"codex-router-zai-coding"}}},
			{ID: "opencode-go", DisplayName: "opencode Go", Kind: "openai-compatible",
				OwnedBy: "opencode", BaseURL: "https://opencode.ai/zen/go/v1", BaseURLEnv: "OPENCODE_GO_BASE_URL",
				Credential: registry.Credential{Environment: []string{"OPENCODE_API_KEY"},
					File: "opencode-go-api-key.secret", KeychainServices: []string{"codex-router-opencode-go"}}},
			{ID: "opencode-go-responses", DisplayName: "opencode Responses", Kind: "openai-compatible",
				OwnedBy: "opencode", VariantOf: "opencode-go", Protocol: "openai-responses",
				BaseURL: "https://opencode.ai/zen/go/v1", BaseURLEnv: "OPENCODE_GO_BASE_URL",
				Credential: registry.Credential{Environment: []string{"OPENCODE_API_KEY"},
					File: "opencode-go-api-key.secret", KeychainServices: []string{"codex-router-opencode-go"}}},
		},
		[]registry.Model{
			{Slug: "zai-coding/glm-5.3", GatewayModel: "zai-coding-glm-5-3", UpstreamModel: "glm-5.3",
				Provider: "zai-coding", Listed: true, DisplayName: "GLM-5.3",
				RequestProfile:  "glm-thinking",
				ReasoningLevels: []registry.ReasoningLevel{{Effort: "low"}, {Effort: "high"}, {Effort: "max"}},
				ContextWindow:   200000},
			{Slug: "opencode-go/glm-5.3", GatewayModel: "opencode-go-glm-5-3", UpstreamModel: "glm-5.3",
				Provider: "opencode-go", Listed: true, DisplayName: "GLM-5.3 opencode",
				ReasoningLevels: []registry.ReasoningLevel{{Effort: "high"}, {Effort: "max"}},
				ContextWindow:   200000},
			{Slug: "opencode-go-responses/gpt-5.6-luna", GatewayModel: "opencode-go-responses-gpt-5-6-luna",
				UpstreamModel: "gpt-5.6-luna", Provider: "opencode-go-responses", Listed: true,
				DisplayName: "GPT 5.6 Luna"},
		},
	)
}

// ---- 认证 ----

func TestAuthentication(t *testing.T) {
	srv, ts := newTestServer(t)
	callerKey, _ := srv.opt.State.CallerKey()

	cases := []struct {
		path string
		code int
	}{
		{CallerPathPrefix + "/" + callerKey + "/v1/responses", 0}, // 认证通过（后续 405/400 均非 401）
		{CallerPathPrefix + "/wrong-key-wrong-key-wrong-key/v1/responses", 401},
		{"/responses", 401},
		{CallerPathPrefix + "/" + callerKey, 401},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+tc.path,
			strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if tc.code == 0 {
			if resp.StatusCode == http.StatusUnauthorized {
				t.Errorf("path %s should authenticate", tc.path)
			}
		} else if resp.StatusCode != tc.code {
			t.Errorf("path %s = %d, want %d", tc.path, resp.StatusCode, tc.code)
		}
	}
}

// 浏览器 Origin 拒绝。
func TestBrowserOriginRejected(t *testing.T) {
	srv, ts := newTestServer(t)
	callerKey, _ := srv.opt.State.CallerKey()
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("browser origin should be 403, got %d", resp.StatusCode)
	}
}

// /health 无认证且带 activity。
func TestHealthUnauthenticated(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["service"] != "codex-router" {
		t.Errorf("service name wrong: %v", payload["service"])
	}
	if _, ok := payload["activity"].(map[string]any); !ok {
		t.Error("health must carry activity payload for the tray")
	}
}

// 端到端：chat 翻译路径（zai-coding）—— 上游收到的请求形态与
// Codex 收到的 SSE 事件序列。
func TestEndToEndChatTranslation(t *testing.T) {
	var upstreamBody map[string]any
	var upstreamAuth string
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	// 把 zai-coding 指到测试上游。
	reg := srv.opt.Registry
	reg.Providers["zai-coding"].BaseURL = upstream.URL
	reg.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")

	callerKey, _ := srv.opt.State.CallerKey()
	reqBody := `{
		"model": "zai-coding/glm-5.3",
		"instructions": "sys prompt",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
		"tools": [{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"reasoning": {"effort": "max"},
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// 上游侧断言。
	if upstreamPath != "/chat/completions" {
		t.Errorf("upstream path = %s, want /chat/completions", upstreamPath)
	}
	if upstreamAuth != "Bearer test-zai-key" {
		t.Errorf("upstream auth = %q", upstreamAuth)
	}
	if upstreamBody["model"] != "glm-5.3" {
		t.Errorf("upstream model = %v (want upstream id)", upstreamBody["model"])
	}
	if thinking, ok := upstreamBody["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
		t.Errorf("glm-thinking must enable thinking, got %v", upstreamBody["thinking"])
	}
	if upstreamBody["reasoning_effort"] != "max" {
		t.Errorf("effort should clamp to max, got %v", upstreamBody["reasoning_effort"])
	}
	if _, ok := upstreamBody["temperature"]; ok {
		t.Error("glm-thinking must drop temperature")
	}
	messages := upstreamBody["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Error("instructions should lead as system message")
	}

	// Codex 侧断言：SSE 事件序列是 Responses 形态。
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	var events []string
	var sawCompleted bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var evt map[string]any
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				t.Fatalf("bad SSE payload: %v (%s)", err, payload)
			}
			if typ, ok := evt["type"].(string); ok {
				events = append(events, typ)
				if typ == "response.completed" {
					sawCompleted = true
					usage := evt["response"].(map[string]any)["usage"].(map[string]any)
					if usage["input_tokens"] != float64(3) {
						t.Errorf("completed usage wrong: %v", usage)
					}
				}
			}
		}
	}
	if !sawCompleted {
		t.Fatalf("no response.completed; events = %v", events)
	}
	if events[0] != "response.created" {
		t.Errorf("first event = %s, want response.created", events[0])
	}
	has := func(name string) bool {
		for _, e := range events {
			if e == name {
				return true
			}
		}
		return false
	}
	if !has("response.output_text.delta") {
		t.Errorf("missing output_text.delta: %v", events)
	}
	if !has("response.output_item.done") {
		t.Errorf("missing output_item.done: %v", events)
	}
}

// native 分流：未注册模型直连 native 后端（用 httptest 顶替）。
func TestNativePassthroughUnregisteredModel(t *testing.T) {
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("native path = %s, want /responses (v1 stripped)", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer codex-session-token" {
			t.Errorf("native must forward caller's session, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n")
	}))
	defer native.Close()

	srv, ts := newTestServer(t)
	srv.setNativeBase(native.URL)

	callerKey, _ := srv.opt.State.CallerKey()
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5.5","input":"hi","store":true,"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer codex-session-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() || !strings.Contains(scanner.Text(), "response.created") {
		t.Errorf("native stream should relay untouched, got %q", scanner.Text())
	}
}

// Responses 直通路径：model 还原、凭据注入、协议字节不动。
func TestResponsesPassthrough(t *testing.T) {
	var upstreamModel string
	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		upstreamModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	reg := srv.opt.Registry
	reg.Providers["opencode-go-responses"].BaseURL = upstream.URL
	reg.Providers["opencode-go-responses"].BaseURLEnv = ""
	t.Setenv("OPENCODE_API_KEY", "")

	callerKey, _ := srv.opt.State.CallerKey()
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"opencode-go-responses/gpt-5.6-luna","input":"hi","stream":true,"reasoning":{"effort":"high"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if upstreamModel != "gpt-5.6-luna" {
		t.Errorf("upstream model = %q, want upstream id", upstreamModel)
	}
	if upstreamAuth != "Bearer test-opencode-key" {
		t.Errorf("upstream auth = %q", upstreamAuth)
	}
}

// 旧版本曾把 custom_tool_call 的 item id 生成为 fc_；native 严格校验要求
// ctc_。透传边界应修复已存在会话里的旧历史，避免用户必须新建任务。
func TestResponsesPassthroughNormalizesLegacyCustomToolIDs(t *testing.T) {
	var input []any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		input, _ = body["input"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	reg := srv.opt.Registry
	reg.Providers["opencode-go-responses"].BaseURL = upstream.URL
	reg.Providers["opencode-go-responses"].BaseURLEnv = ""
	t.Setenv("OPENCODE_API_KEY", "")

	callerKey, _ := srv.opt.State.CallerKey()
	body := `{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"input":[{"type":"custom_tool_call","id":"fc_legacy","call_id":"call_legacy","name":"exec","input":"ls"},{"type":"custom_tool_call_output","id":"fc_legacy_output","call_id":"call_legacy","output":"ok"}]}`
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(input) != 2 {
		t.Fatalf("upstream input length = %d, want 2", len(input))
	}
	if got := input[0].(map[string]any)["id"]; got != "ctc_legacy" {
		t.Errorf("custom tool call id = %v, want ctc_legacy", got)
	}
	if got := input[1].(map[string]any)["id"]; got != "ctco_legacy_output" {
		t.Errorf("custom tool output id = %v, want ctco_legacy_output", got)
	}
}

func TestNormalizeLegacyCustomToolFrame(t *testing.T) {
	frame := []byte(`{"type":"response.create","response":{"input":[{"type":"custom_tool_call","id":"fc_ws","call_id":"call_ws","name":"exec","input":"ls"}]}}`)
	normalized := normalizeLegacyCustomToolFrame(frame)
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		t.Fatal(err)
	}
	response := payload["response"].(map[string]any)
	items := response["input"].([]any)
	if got := items[0].(map[string]any)["id"]; got != "ctc_ws" {
		t.Errorf("websocket custom tool call id = %v, want ctc_ws", got)
	}
}

// 未启用 provider 的注册模型 → 409。
func TestProviderNotEnabled(t *testing.T) {
	srv, ts := newTestServer(t)
	if err := srv.opt.State.SetEnabledProviders([]string{"opencode-go"}); err != nil {
		t.Fatal(err)
	}
	callerKey, _ := srv.opt.State.CallerKey()
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("hidden provider should 409, got %d", resp.StatusCode)
	}
}

// WS 升级握手必须回 426 Upgrade Required —— 这是 Codex 客户端设计的
// "干净回退"信号（拿到 426 不重试、当轮直接切 HTTP 并会话级粘住）。
// 回 404 会被当普通流错误烧满 5 次重试（实测新线程首轮 ~7 秒 +
// "正在重新连接 5/5" 横幅）。
func TestWebSocketUpgradeReturnsUpgradeRequired(t *testing.T) {
	srv, ts := newTestServer(t)
	callerKey, _ := srv.opt.State.CallerKey()
	path := CallerPathPrefix + "/" + callerKey + "/v1/responses"

	// ① 标准 RFC 6455 握手 → 426，带 Upgrade 头（RFC 要求 426 携带）。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("websocket upgrade handshake = %d, want 426", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("426 response Upgrade header = %q, want websocket", got)
	}

	// ② 无升级头的普通 GET → 维持原 404（不误吞其他 GET 探测）。
	req, _ = http.NewRequest(http.MethodGet, ts.URL+path, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("plain GET = %d, want 404", resp.StatusCode)
	}

	// ③ 错误 caller key 的握手 → 认证先行，401。
	req, _ = http.NewRequest(http.MethodGet,
		ts.URL+CallerPathPrefix+"/wrong-key-wrong-key-wrong-key/v1/responses", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("websocket upgrade with bad key = %d, want 401", resp.StatusCode)
	}
}
