// idle.go —— 上游响应体的空闲看门狗。
//
// 背景（2026-08-16 两次上游黑洞）：上游接受请求后可能既不吐字节也
// 不报错也不断开（代理链路黑洞或上游挂死）。响应已开始后 body 上的
// 无限期阻塞没有其他机制兜底；此处把"无限挂起"变成"超时断开 +
// 可识别错误"，让 Codex 决定是否重试。
package httpx

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrUpstreamIdle 是看门狗触发的哨兵：响应体在 IdleTimeout 窗口内
// 没有产出任何字节。调用方用 errors.Is 识别后可映射为 504。
var ErrUpstreamIdle = errors.New("upstream idle timeout")

// idleReadCloser 包装一个响应体：每次成功读到字节就重置计时器；
// 连续 d 无字节则关闭底层 body（解除阻塞中的 Read）并把后续/当前
// Read 的错误替换为 ErrUpstreamIdle 包装。
type idleReadCloser struct {
	rc      io.ReadCloser
	timeout time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	tripped bool
	closed  bool
}

// NewIdleReadCloser 包裹 rc：d <= 0 时原样返回（看门狗关闭）。
func NewIdleReadCloser(rc io.ReadCloser, d time.Duration) io.ReadCloser {
	if rc == nil || d <= 0 {
		return rc
	}
	w := &idleReadCloser{rc: rc, timeout: d}
	// 构造期无并发，直接武装首次计时器。
	w.resetLocked()
	return w
}

// reset 重新武装计时器。调用方需持锁。
func (w *idleReadCloser) resetLocked() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.timeout, w.trip)
}

// trip 是超时回调：关底层 body 解除阻塞中的 Read，并打标记。
func (w *idleReadCloser) trip() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.tripped {
		return
	}
	w.tripped = true
	// body.Close() 让阻塞中的 Read 立刻以错误返回；已结束的 Read
	// 循环则在下一次 Read 时拿到错误。
	_ = w.rc.Close()
}

func (w *idleReadCloser) Read(p []byte) (int, error) {
	w.mu.Lock()
	tripped := w.tripped
	w.mu.Unlock()
	if tripped {
		return 0, fmt.Errorf("read aborted after %v of silence: %w", w.timeout, ErrUpstreamIdle)
	}
	n, err := w.rc.Read(p)
	if n > 0 {
		// 有字节流动：重置计时器（仍活着）。
		w.mu.Lock()
		if !w.closed && !w.tripped {
			w.resetLocked()
		}
		w.mu.Unlock()
	}
	if err != nil {
		w.mu.Lock()
		idle := w.tripped
		w.mu.Unlock()
		if idle {
			return n, fmt.Errorf("read aborted after %v of silence: %w", w.timeout, ErrUpstreamIdle)
		}
	}
	return n, err
}

func (w *idleReadCloser) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
	return w.rc.Close()
}
