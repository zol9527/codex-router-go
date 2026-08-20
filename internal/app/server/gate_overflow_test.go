package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loyd/codex-router/internal/domain/registry"
)

// P0-2（opencode v2 借鉴）：头已提交 + 中途断流 + 流中已有工具调用 →
// 必须以 response.failed 显式收尾，不得静默截断（整轮重试会重复执行
// 已流出的工具调用）。纯文本流断流维持静默截断（重试无副作用）。
func TestInterruptedToolCallStreamFailsExplicitly(t *testing.T) {
	cases := []struct {
		name        string
		delta       string // 首个活性 delta
		wantFailed  bool
		description string
	}{
		{
			name:        "tool call then abrupt disconnect",
			delta:       `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]}}]}` + "\n\n",
			wantFailed:  true,
			description: "stream_interrupted_after_tool_call",
		},
		{
			name:        "text then abrupt disconnect",
			delta:       `data: {"choices":[{"delta":{"content":"partial text"}}]}` + "\n\n",
			wantFailed:  false,
			description: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				fmt.Fprint(w, tc.delta)
				flusher.Flush()
				// 模拟中途断流：连接层异常（读侧得到非 EOF 错误）。
				panic(http.ErrAbortHandler)
			}))
			defer upstream.Close()

			srv, ts := newTestServer(t)
			srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
			srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
			t.Setenv("ZAI_API_KEY", "")
			callerKey, _ := srv.opt.State.CallerKey()

			req, _ := http.NewRequest(http.MethodPost,
				ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
				strings.NewReader(`{"model":"zai-coding/glm-5.3","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			done := make(chan string, 1)
			go func() {
				buf := make([]byte, 32<<10)
				var out strings.Builder
				for {
					n, rerr := resp.Body.Read(buf)
					out.Write(buf[:n])
					if rerr != nil {
						done <- out.String()
						return
					}
				}
			}()
			select {
			case body := <-done:
				if tc.wantFailed {
					if !strings.Contains(body, "response.failed") {
						t.Errorf("tool-call stream interrupted must end with response.failed, got %.300s", body)
					}
					if !strings.Contains(body, tc.description) {
						t.Errorf("response.failed must carry code %q, got %.300s", tc.description, body)
					}
				} else if strings.Contains(body, "response.failed") {
					t.Errorf("text-only interrupted stream must stay silently truncated (client retry is side-effect free), got response.failed")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("test timed out waiting for stream end")
			}
		})
	}
}

// P1-1（opencode v2 借鉴）：上游超窗错误规范化为 OpenAI 官方 code
// context_length_exceeded；限流文案不得误判。
func TestClassifyContextOverflow(t *testing.T) {
	positive := []string{
		"This model's maximum context length is 128000 tokens. However, you requested 200000 tokens.",
		"context_length_exceeded: your input is too long",
		"the prompt is too long: 200000 tokens > 128000 maximum",
		"Input length exceeds maximum token limit",
		"请求过长：超过模型上下文窗口限制",
		"Please reduce the length of the messages.",
	}
	for _, s := range positive {
		if !classifyContextOverflow(s) {
			t.Errorf("should classify as overflow: %q", s)
		}
	}
	negative := []string{
		"Rate limit hit: too many requests",
		"Too many requests in one minute",
		"You exceeded your current quota",
		"invalid request: unknown parameter",
		"upstream 500: internal error",
	}
	for _, s := range negative {
		if classifyContextOverflow(s) {
			t.Errorf("should NOT classify as overflow: %q", s)
		}
	}
}

func TestTranslateProviderErrorContextOverflow(t *testing.T) {
	body := translateProviderError(400,
		`{"error":{"message":"This model's maximum context length is 128000 tokens. However, you requested 200000 tokens.","type":"invalid_request_error"}}`,
		"zai-coding/glm-5.3", "Z.ai GLM Coding Plan", "zai-coding", 0)
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "context_length_exceeded" {
		t.Errorf("code = %q, want context_length_exceeded", code)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "compact the session") {
		t.Errorf("message should carry compaction guidance, got %q", msg)
	}
}

// 回归：quota 分类优先级高于 overflow（两者文案可能同时命中时
// 配额处置指引更重要）。
func TestQuotaWinsOverOverflow(t *testing.T) {
	body := translateProviderError(429,
		`{"error":{"message":"usage limit reached, your prompt exceeds quota"}}`,
		"m", "p", "pid", 0)
	errObj, _ := body["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "provider_quota_exhausted" {
		t.Errorf("quota must win, got %q", code)
	}
}

var _ = registry.Model{} // 保持 registry 引用（与实现文件的一致性检查）。
