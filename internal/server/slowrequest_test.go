package server

import (
	"bytes"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer 是并发安全的日志收集器：看门狗在独立 goroutine 里
// log.Printf，测试主协程同时轮询读取，裸 bytes.Buffer 会数据竞争。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog 把全局 logger 接到收集器，测试结束还原。注意必须显式还原
// 成 os.Stderr —— SetOutput(nil) 会把输出置成 nil Writer，之后任何
// logf（含 http.Server 的 panic 恢复日志）都是空指针崩溃。
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// newBareWatchdogServer 构造只够 beginRequest 跑起来的最小 Server：
// 该路径只碰 active/requestSeq/slowRequestLog，无需 state 与注册表。
func newBareWatchdogServer(delay time.Duration) *Server {
	return &Server{
		active:         map[int]*activityEntry{},
		slowRequestLog: delay,
	}
}

// 挂死请求（不 finish）超过窗口必须落一行 slow request pending，
// 且带上 setRoute 登记的 provider/model —— 这是 2026-08-16 TLS 黑洞
// 事故里缺失的那行日志。
func TestBeginRequestSlowWatchdogLogsPending(t *testing.T) {
	buf := captureLog(t)
	srv := newBareWatchdogServer(40 * time.Millisecond)

	setRoute, _ := srv.beginRequest()
	setRoute("openai", "gpt-5.6-luna", "commit-message")

	waitFor(t, "slow request pending log line", func() bool {
		return strings.Contains(buf.String(), "slow request pending") &&
			strings.Contains(buf.String(), "gpt-5.6-luna")
	})
}

// 窗口内正常收尾（finish）的请求不许误报；条目也要从 active 摘除。
func TestBeginRequestWatchdogSilentAfterFinish(t *testing.T) {
	buf := captureLog(t)
	srv := newBareWatchdogServer(150 * time.Millisecond)

	setRoute, finish := srv.beginRequest()
	setRoute("openai", "gpt-5.6-luna", "")
	finish(200)

	// 等过窗口再断言，确认看门狗确实没有触发。
	time.Sleep(250 * time.Millisecond)
	if got := buf.String(); strings.Contains(got, "slow request pending") {
		t.Fatalf("finished request must not be reported as slow, log: %s", got)
	}
	srv.mu.Lock()
	remaining := len(srv.active)
	srv.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("active map should be empty after finish, got %d entries", remaining)
	}
}

// 窗口设为 0（env 设 0 关闭）时不得启动看门狗 —— AfterFunc(0) 会立刻
// 触发，把每个请求都误报成慢请求。
func TestBeginRequestWatchdogDisabled(t *testing.T) {
	buf := captureLog(t)
	srv := newBareWatchdogServer(0)

	_, finish := srv.beginRequest()
	time.Sleep(80 * time.Millisecond)
	if got := buf.String(); strings.Contains(got, "slow request pending") {
		t.Fatalf("disabled watchdog must stay silent, log: %s", got)
	}
	finish(200)
}
