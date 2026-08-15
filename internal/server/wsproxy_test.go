package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startWSMockUpstream 起一个"原生后端"：在 /responses 接受升级，把每条
// 文本帧回显为 response.created + response.completed 两条事件（Codex 的
// 流以 completed 收尾），并记录收到的帧。
func startWSMockUpstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	received := &[]string{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mtype, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			mu.Lock()
			*received = append(*received, string(data))
			mu.Unlock()
			if err := conn.WriteMessage(mtype, []byte(`{"type":"response.created","response":{"id":"resp-mock"}}`)); err != nil {
				return
			}
			if err := conn.WriteMessage(mtype, []byte(`{"type":"response.completed","response":{"id":"resp-mock"}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts, received
}

// wsRouterURL 拼出路由器侧 WS 端点（httptest http URL → ws）。
func wsRouterURL(ts *httptest.Server, callerKey string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") +
		CallerPathPrefix + "/" + callerKey + "/v1/responses"
}

// waitFor 轮询断言，超时 fatal。
func waitFor(t *testing.T, desc string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", desc)
}

// 端到端：原生 hint → 101 升级 → 帧经管道往返上游。
func TestWebSocketNativePassthroughPipe(t *testing.T) {
	upstream, received := startWSMockUpstream(t)
	srv, ts := newTestServer(t)
	srv.opt.NativeBase = upstream.URL

	callerKey, _ := srv.opt.State.CallerKey()
	header := http.Header{}
	header.Set("X-Codex-Routing-Hint", "model=gpt-5.6-sol")
	conn, resp, err := websocket.DefaultDialer.Dial(wsRouterURL(ts, callerKey), header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("native hint should upgrade and pipe, got status=%d err=%v", status, err)
	}
	defer conn.Close()

	frame := `{"type":"response.create","model":"gpt-5.6-sol","stream":true}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("should receive relayed upstream event: %v", err)
	}
	if !strings.Contains(string(first), "response.created") {
		t.Errorf("first relayed event = %s, want response.created", first)
	}
	waitFor(t, "upstream receives the frame verbatim", func() bool {
		for _, got := range *received {
			if got == frame {
				return true
			}
		}
		return false
	})
}

// 路由 hint → 426（Codex 的干净回退信号），不建立管道。
func TestWebSocketRoutedHintFallsBack(t *testing.T) {
	upstream, _ := startWSMockUpstream(t)
	srv, ts := newTestServer(t)
	srv.opt.NativeBase = upstream.URL

	callerKey, _ := srv.opt.State.CallerKey()
	header := http.Header{}
	header.Set("X-Codex-Routing-Hint", "model=zai-coding/glm-5.3")
	conn, resp, err := websocket.DefaultDialer.Dial(wsRouterURL(ts, callerKey), header)
	if err == nil {
		conn.Close()
		t.Fatal("routed hint must not establish a pipe")
	}
	if resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("routed hint handshake = %v, want 426", resp)
	}
	if got := resp.Header.Get("Upgrade"); got != "websocket" {
		t.Errorf("426 Upgrade header = %q, want websocket", got)
	}
}

// 上游失联 → 同样 426 回退（调用方干净切 HTTP，而非死管道）。
func TestWebSocketUpstreamDownFallsBack(t *testing.T) {
	srv, ts := newTestServer(t)
	// 端口 1 基本必拒连，避免依赖外部 DNS。
	srv.opt.NativeBase = "http://127.0.0.1:1"

	callerKey, _ := srv.opt.State.CallerKey()
	header := http.Header{}
	header.Set("X-Codex-Routing-Hint", "model=gpt-5.6-sol")
	conn, resp, err := websocket.DefaultDialer.Dial(wsRouterURL(ts, callerKey), header)
	if err == nil {
		conn.Close()
		t.Fatal("dead upstream must not establish a pipe")
	}
	if resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("dead upstream handshake = %v, want 426", resp)
	}
}

// 管道上出现路由帧（线程中途换模型的边缘情况）→ 服务端主动断线，
// 调用方带 hint 重连将命中 426 自愈。
func TestWebSocketRoutedFrameOnPipeCloses(t *testing.T) {
	upstream, _ := startWSMockUpstream(t)
	srv, ts := newTestServer(t)
	srv.opt.NativeBase = upstream.URL

	callerKey, _ := srv.opt.State.CallerKey()
	header := http.Header{}
	header.Set("X-Codex-Routing-Hint", "model=gpt-5.6-sol")
	conn, _, err := websocket.DefaultDialer.Dial(wsRouterURL(ts, callerKey), header)
	if err != nil {
		t.Fatalf("native hint should upgrade: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"response.create","model":"zai-coding/glm-5.3"}`)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok &&
				closeErr.Code == websocket.ClosePolicyViolation {
				return // 预期：策略违规关闭
			}
			t.Fatalf("want close 1008, got %v", err)
		}
	}
}

// 回滚开关：关闭透传后原生 hint 也一律 426。
func TestWebSocketPassthroughDisabled(t *testing.T) {
	upstream, _ := startWSMockUpstream(t)
	srv, ts := newTestServer(t)
	srv.opt.NativeBase = upstream.URL
	srv.opt.DisableWebSocketPassthrough = true

	callerKey, _ := srv.opt.State.CallerKey()
	header := http.Header{}
	header.Set("X-Codex-Routing-Hint", "model=gpt-5.6-sol")
	conn, resp, err := websocket.DefaultDialer.Dial(wsRouterURL(ts, callerKey), header)
	if err == nil {
		conn.Close()
		t.Fatal("disabled passthrough must not establish a pipe")
	}
	if resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("disabled handshake = %v, want 426", resp)
	}
}
