package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loyd/codex-router/internal/domain/usage"
)

// 看门狗（响应体空闲）：上游发头后一字节不吐（2026-08-16 14:04 黑洞
// 的形状）→ 头未提交时回 504 + upstream_idle_timeout，Codex 自带重试
// 接管；usage 记录 upstreamIdle。
func TestIdleWatchdogBeforeLivenessReturns504(t *testing.T) {
	block := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-block // 头已发、body 永不产出
	}))
	// LIFO：先放行 handler，再关服务（Close 会等存量 handler 退出）。
	defer upstream.Close()
	defer close(block)

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	srv.upstreamIdle = 150 * time.Millisecond
	callerKey, _ := srv.opt.State.CallerKey()
	recorder := usage.NewRecorder(t.TempDir())
	srv.opt.Usage = recorder

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("idle before liveness must 504, got %d", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "upstream_idle_timeout") {
		t.Errorf("error type must name the watchdog:\n%s", body)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("watchdog must fail fast, took %v", elapsed)
	}
	if raw := readUsageRaw(t, recorder); !strings.Contains(raw, `"upstreamIdle":true`) {
		t.Errorf("usage must record upstreamIdle: %s", raw)
	}
}

// 看门狗（响应体空闲）：liveness（reasoning）已过、头已提交后上游挂死
// → 只能截断（不得 writeJSON 二次提交头）；client 收到部分内容后流
// 结束，且 Router 不会追加请求。
func TestIdleWatchdogMidStreamTruncates(t *testing.T) {
	var calls int
	block := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking hard\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-block // 活性之后挂死
	}))
	defer upstream.Close()
	defer close(block)

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	srv.upstreamIdle = 150 * time.Millisecond
	callerKey, _ := srv.opt.State.CallerKey()
	recorder := usage.NewRecorder(t.TempDir())
	srv.opt.Usage = recorder

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("head was committed as 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("SSE content type expected, got %q", ct)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "thinking hard") {
		t.Errorf("partial content must reach the client:\n%s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Errorf("truncated stream must not carry a completion event:\n%s", body)
	}
	if calls != 1 {
		t.Errorf("liveness proven, Router must not append a request, calls=%d", calls)
	}
	if raw := readUsageRaw(t, recorder); !strings.Contains(raw, `"streamAborted":true`) {
		t.Errorf("usage must record streamAborted: %s", raw)
	}
}

// 响应头超时：上游迟迟不回响应头（挂死在 Do 阶段）→ 快速失败 502，
// 而不是陪上游无限等（此前 zai 黑洞 505s 才回 500）。
func TestHeaderTimeoutFailsFast(t *testing.T) {
	block := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 连响应头都不发
	}))
	defer upstream.Close()
	defer close(block)

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	// Transport 在 New() 时已定型，测试直接换一个短窗口的。
	srv.client.Transport = &http.Transport{ResponseHeaderTimeout: 200 * time.Millisecond}
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("header timeout must fail as 502, got %d", resp.StatusCode)
	}
	// 3 次尝试（1 + 2 重试）× 200ms + 退避 ≈ 1.6s，远小于无限等待。
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Errorf("header timeout must fail fast, took %v", elapsed)
	}
}
