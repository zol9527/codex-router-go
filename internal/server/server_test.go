package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
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
	srv.opt.NativeBase = native.URL

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
