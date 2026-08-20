package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---- frameTurnDone ----

func TestFrameTurnDone(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  bool
	}{
		{"completed", `{"type":"response.completed","response":{"id":"r"}}`, true},
		{"failed", `{"type":"response.failed","response":{"id":"r"}}`, true},
		{"incomplete", `{"type":"response.incomplete","response":{"id":"r"}}`, true},
		{"cancelled", `{"type":"response.cancelled","response":{"id":"r"}}`, true},
		{"created 是过程事件", `{"type":"response.created","response":{"id":"r"}}`, false},
		{"流式 delta", `{"type":"response.output_text.delta","delta":"hi"}`, false},
		{"无 type 字段", `{"model":"gpt-5.6-sol"}`, false},
		{"畸形 JSON", `{not-json`, false},
		{"空帧", ``, false},
	}
	for _, tc := range cases {
		if got := frameTurnDone([]byte(tc.frame)); got != tc.want {
			t.Errorf("frameTurnDone(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---- wsWatchdog 在途 turn 状态机 ----

func TestWSWatchdogTurnStateMachine(t *testing.T) {
	wd := &wsWatchdog{timeout: time.Minute}

	if wd.inFlight() {
		t.Fatal("新管道（客户端未发帧）不得视为在途 turn")
	}
	wd.noteClient() // response.create
	if !wd.inFlight() {
		t.Fatal("客户端请求帧后必须视为在途 turn")
	}
	wd.noteUpstreamTurnDone([]byte(`{"type":"response.created"}`))
	if !wd.inFlight() {
		t.Fatal("过程事件（created/delta）不得结束 turn")
	}
	wd.noteUpstreamTurnDone([]byte(`{"type":"response.completed"}`))
	if wd.inFlight() {
		t.Fatal("结束事件后 turn 必须退出在途")
	}

	// 同一管道上的下一个 turn：请求帧重新置位。
	wd.noteClient()
	if !wd.inFlight() {
		t.Fatal("管道复用的第二个 turn 必须重新置位在途")
	}
}

// nil 接收者安全：beginActivity 挂进 entry 的回调在极端时序下可能被
// nil 管道调用，不得 panic。
func TestWSWatchdogInFlightNilSafe(t *testing.T) {
	var wd *wsWatchdog
	if wd.inFlight() {
		t.Fatal("nil watchdog must report not in flight")
	}
}

// ---- activity 上报语义 ----

// 空闲管道（turn 已收尾、管道未关）不得计入 active / generating ——
// 这是 2026-08-18 "对话几次后 state 永远 generating" 的核心回归测试。
func TestActivityPayloadIdlePipeNotGenerating(t *testing.T) {
	srv := newBareWatchdogServer(0)
	wd := &wsWatchdog{timeout: time.Minute}

	setRoute, _ := srv.beginActivity(wd.inFlight)
	setRoute("openai", "gpt-5.6-luna", "demo")

	wd.noteClient() // 请求帧 → 在途
	if state := srv.activityPayload()["state"]; state != "generating" {
		t.Fatalf("turn 在途时 state = %v, want generating", state)
	}
	if count := srv.activityPayload()["activeCount"]; count != 1 {
		t.Fatalf("turn 在途时 activeCount = %v, want 1", count)
	}

	wd.noteUpstreamTurnDone([]byte(`{"type":"response.completed"}`))
	payload := srv.activityPayload()
	if payload["state"] != "idle" {
		t.Fatalf("turn 收尾后 state = %v, want idle（管道未关也不得报 generating）", payload["state"])
	}
	if payload["activeCount"] != 0 {
		t.Fatalf("turn 收尾后 activeCount = %v, want 0", payload["activeCount"])
	}

	// 管道级 entry 仍留在 map 里等拆管 finish（不能因 idle 被踢出，
	// 否则管道后续 turn 不再上报）。
	srv.mu.Lock()
	remaining := len(srv.active)
	srv.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("idle 管道的 entry 应保留在 map 等待拆管，got %d entries", remaining)
	}
}

// stale 清理只针对 HTTP 条目；带 inFlight 的管道条目由拆管收尾，
// 按 startedAt 过期会把健康管道永久踢出上报。
func TestActivityPayloadStaleSkipsPipeEntries(t *testing.T) {
	srv := newBareWatchdogServer(0)
	srv.mu.Lock()
	srv.active[1] = &activityEntry{
		id: "1", provider: "openai", model: "gpt-5.6-luna",
		startedAt: time.Now().Add(-2 * staleActivity), inFlight: func() bool { return true },
	}
	srv.active[2] = &activityEntry{
		id: "2", provider: "openai", model: "gpt-5.6-terra",
		startedAt: time.Now().Add(-2 * staleActivity), // HTTP 条目：无 inFlight
	}
	srv.mu.Unlock()

	payload := srv.activityPayload()
	if payload["state"] != "generating" || payload["activeCount"] != 1 {
		t.Fatalf("过期的 HTTP 条目应被清、在途管道条目应保留: state=%v count=%v",
			payload["state"], payload["activeCount"])
	}
	srv.mu.Lock()
	remaining := len(srv.active)
	srv.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("stale 清理后 map 应只剩管道条目，got %d entries", remaining)
	}
}

// 空闲管道不是慢请求：slow request pending 看门狗对无在途 turn 的
// 管道条目保持沉默（2026-08-18 之前每条挂着的管道都会刷一行误报）。
func TestSlowWatchdogSilentForIdlePipe(t *testing.T) {
	buf := captureLog(t)
	srv := newBareWatchdogServer(40 * time.Millisecond)
	wd := &wsWatchdog{timeout: time.Minute}

	setRoute, _ := srv.beginActivity(wd.inFlight)
	setRoute("openai", "gpt-5.6-luna", "demo")
	// turn 已收尾、管道挂着不关。
	wd.noteClient()
	wd.noteUpstreamTurnDone([]byte(`{"type":"response.completed"}`))

	time.Sleep(150 * time.Millisecond)
	if got := buf.String(); strings.Contains(got, "slow request pending") {
		t.Fatalf("空闲管道不得报慢请求, log: %s", got)
	}

	// 对照：在途 turn 超过窗口仍要报（上游黑洞可见性）。看门狗是登记
	// 起一次性触发的 timer，用新 entry 驱动。
	wdStuck := &wsWatchdog{timeout: time.Minute}
	setRouteStuck, _ := srv.beginActivity(wdStuck.inFlight)
	setRouteStuck("openai", "gpt-5.6-luna", "demo-stuck")
	wdStuck.noteClient() // 在途且永不收尾
	waitFor(t, "slow request pending for stuck in-flight turn", func() bool {
		return strings.Contains(buf.String(), "slow request pending") &&
			strings.Contains(buf.String(), "demo-stuck")
	})
}

// ---- 端到端：真实管道上的 activity 生命周期 ----

// 管道上的 turn 收尾后（客户端收到 completed、连接保持打开），
// /health 语义必须回到 idle —— 对应 Codex 每对话一条 preconnect、
// 对话结束后管道长期不关的真实行为。
func TestWSPipeActivityFollowsTurnEndToEnd(t *testing.T) {
	upstream, _ := startWSMockUpstream(t)
	srv, ts := newTestServer(t)
	srv.setNativeBase(upstream.URL)

	callerKey, _ := srv.opt.State.CallerKey()
	header := http.Header{}
	header.Set("X-Codex-Routing-Hint", "model=gpt-5.6-sol")
	conn, _, err := websocket.DefaultDialer.Dial(wsRouterURL(ts, callerKey), header)
	if err != nil {
		t.Fatalf("native hint should upgrade: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"response.create","model":"gpt-5.6-sol","stream":true}`)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 2; i++ { // created + completed
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read relayed event: %v", err)
		}
	}
	conn.SetReadDeadline(time.Time{})

	waitFor(t, "activity falls back to idle after turn completed", func() bool {
		return srv.activityPayload()["state"] == "idle"
	})
}
