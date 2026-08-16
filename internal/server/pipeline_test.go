package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/spill"
	"github.com/loyd/codex-router/internal/usage"
)

// 空补全守卫：静默空流 → 隐形重试 → 第二次成功，client 只见一份响应。
func TestEmptyCompletionSilentRetry(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			// 空流：无 content、无 reasoning，直接终止。
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Recovered.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")

	callerKey, _ := srv.opt.State.CallerKey()
	recorder := usage.NewRecorder(t.TempDir())
	srv.opt.Usage = recorder

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if calls != 2 {
		t.Errorf("upstream calls = %d, want 2 (silent retry)", calls)
	}
	body := readAll(t, resp)
	if strings.Count(body, "event: response.created") != 1 {
		t.Errorf("client must see exactly one response head, got:\n%s", body)
	}
	if !strings.Contains(body, "Recovered.") {
		t.Errorf("retry result missing:\n%s", body)
	}
	// usage 记录空补全重试事实。
	raw := readUsageRaw(t, recorder)
	if !strings.Contains(raw, `"emptyCompletionRetried":true`) {
		t.Errorf("usage event must record emptyCompletionRetried: %s", raw)
	}
}

// 守卫红线：liveness（reasoning）出现后不可重试 —— 头已提交。
func TestUnrepairableAfterLiveness(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if calls != 1 {
		t.Errorf("a stream that proved liveness must not retry, calls=%d", calls)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "empty_completion") {
		t.Errorf("must state the failure instead:\n%s", body)
	}
}

// 回归：custom-tool-only 流不产生任何活性事件（custom 调用刻意不发
// function_call_arguments.delta，从 output_item.done 取完整载荷），
// 头从未提交 —— 修复前 handler 静默返回 200 + content-length:0，
// Codex 判 "stream closed before response.completed" 5 连重试耗尽
// （2026-08-16 01:33-01:41 实发，GLM 无思考直接调 apply_patch 的轮次）。
func TestCustomToolOnlyStreamFlushed(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"apply_patch\",\"arguments\":\"*** Begin Patch\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	// 请求声明 custom 工具，上游的 tool_calls 才会被还原成
	// custom_tool_call（无 arguments 增量）。
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3","input":"hi","stream":true,"tools":[{"type":"custom","name":"apply_patch","description":"apply a patch"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// custom 调用本身算内容（HasContent=true），不得触发隐形重试。
	if calls != 1 {
		t.Errorf("custom tool call is content, no silent retry expected, calls=%d", calls)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("SSE head must be committed before handler returns, got content-type %q", ct)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "custom_tool_call") {
		t.Errorf("custom tool call must reach the client:\n%s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Errorf("termination event must reach the client:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("[DONE] sentinel must reach the client:\n%s", body)
	}
}

// 补零替换：上游报 prompt_tokens:0 的大请求，completed 事件携带估算。
func TestZeroPromptTokenSubstitution(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":2,\"total_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()
	recorder := usage.NewRecorder(t.TempDir())
	srv.opt.Usage = recorder

	// 大请求体（超过 1000 token 估算下限）。
	large := strings.Repeat("x", 8*1024)
	reqBody := fmt.Sprintf(`{"model":"zai-coding/glm-5.3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"%s"}]}],"stream":true}`, large)
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
	if !strings.Contains(body, `"input_tokens":2490`) {
		// 8192+ 字节 / 3.3 ≈ 2483+，具体值不重要；断言非零。
		if !strings.Contains(body, `"input_tokens":2`) {
			t.Errorf("explicit zero should be substituted with estimate, got:\n%.400s", body)
		}
	}
	// telemetry 保留 provider 原值 0 + 替换标记。
	raw := readUsageRaw(t, recorder)
	if !strings.Contains(raw, `"estimatedInputTokens":`) {
		t.Errorf("usage event must carry estimatedInputTokens: %s", raw)
	}
	if !strings.Contains(raw, `"inputTokens":0`) {
		t.Errorf("usage event must keep provider-reported zero: %s", raw)
	}
}

// 错误翻译：配额耗尽与套餐不含 API 的分类。
func TestErrorTranslationClassification(t *testing.T) {
	quota := translateProviderError(429,
		`litellm.RateLimitError: RateLimitError: OpenAIException - Insufficient quota. Received Model Group=x`,
		"GLM-5.3", "Z.ai GLM Coding Plan", "zai-coding", 30)
	quotaError := quota["error"].(map[string]any)
	if quotaError["type"] != "provider_quota_exhausted" {
		t.Errorf("quota class = %v", quotaError["type"])
	}
	if !strings.Contains(quotaError["message"].(string), "Z.ai GLM Coding Plan") {
		t.Error("must name the provider")
	}

	entitlement := translateProviderError(403,
		`{"error":{"message":"Your Go plan doesn't include API access. Upgrade to Provider or higher."}}`,
		"GPT", "opencode Go", "opencode-go", 0)
	entError := entitlement["error"].(map[string]any)
	if entError["type"] != "plan_entitlement_required" {
		t.Errorf("entitlement class = %v", entError["type"])
	}
	if strings.Contains(entError["message"].(string), "litellm") {
		t.Error("wrapper prefixes must be stripped")
	}
}

// spill：超阈值工具结果首过境即截断（内容判定，与位置无关），回执带
// 落盘路径；小结果逐字节保留。
func TestSpillInPipeline(t *testing.T) {
	// JSON 字符串内的换行必须转义（裸换行是非法 JSON）。
	bigOutput := strings.ReplaceAll(strings.Repeat("result-line\n", 5000), "\n", "\\n") // ~55KB
	var upstreamInput map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&upstreamInput)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	// 6 个 tool result：判定只看内容 —— 最新的大结果同样被截，
	// 所有小结果逐字节保留。
	var items []string
	for i := 1; i <= 6; i++ {
		items = append(items,
			fmt.Sprintf(`{"type":"function_call","call_id":"c%d","name":"shell","arguments":"{}"}`, i),
			fmt.Sprintf(`{"type":"function_call_output","call_id":"c%d","output":"small-%d"}`, i, i))
	}
	// 第 1 个结果换成大文本（≥32KB → spill 目标，无论新旧）。
	items[1] = fmt.Sprintf(`{"type":"function_call_output","call_id":"c1","output":"%s"}`, bigOutput)
	items = append(items, `{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}`)
	reqBody := fmt.Sprintf(`{"model":"zai-coding/glm-5.3","input":[%s],"stream":true}`,
		strings.Join(items, ","))
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	messages := upstreamInput["messages"].([]any)
	var toolContents []string
	for _, raw := range messages {
		m := raw.(map[string]any)
		if m["role"] == "tool" {
			toolContents = append(toolContents, m["content"].(string))
		}
	}
	if len(toolContents) != 6 {
		t.Fatalf("expected 6 tool results, got %d", len(toolContents))
	}
	// 超阈值的大结果换成回执：截断声明 + 落盘路径。
	if !strings.Contains(toolContents[0], "truncated by Model Router on first transit") {
		t.Errorf("oversized result should be spilled, got %.80s", toolContents[0])
	}
	if !strings.Contains(toolContents[0], srv.opt.State.Dir+string(os.PathSeparator)+spill.DirName) {
		t.Errorf("receipt must point into the spill dir, got %.120s", toolContents[0])
	}
	// 全部小结果逐字节保留。
	for i := 1; i <= 5; i++ {
		if toolContents[i] != fmt.Sprintf("small-%d", i+1) {
			t.Errorf("small result %d must stay byte-for-byte, got %q", i+1, toolContents[i])
		}
	}
	// 回执指向的落盘文件真实存在。
	pathIdx := strings.Index(toolContents[0], "Full output saved to: ")
	spillPath := toolContents[0][pathIdx+len("Full output saved to: "):]
	spillPath = spillPath[:strings.IndexByte(spillPath, '\n')]
	if _, err := os.Stat(spillPath); err != nil {
		t.Errorf("spill file must exist: %v", err)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	var buf strings.Builder
	for scanner.Scan() {
		buf.WriteString(scanner.Text())
		buf.WriteString("\n")
	}
	return buf.String()
}

func readUsageRaw(t *testing.T, recorder *usage.Recorder) string {
	t.Helper()
	path := usagePath(recorder)
	raw, err := readFileOrNull(path)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	return raw
}

func readFileOrNull(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// usagePath 从 recorder 取路径（测试同包访问）。
var usagePath = func(r *usage.Recorder) string { return r.Path() }
