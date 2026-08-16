package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// zstd 编解码往返。
func TestZstdRoundTrip(t *testing.T) {
	body := bytes.Repeat([]byte(`{"input":[{"type":"message","role":"user","content":"hello world, this is a routed codex turn"}]}`), 400)
	compressed, encoding := CompressBody(body)
	if encoding != "zstd" {
		t.Fatalf("large body should compress, got encoding %q", encoding)
	}
	decoded, err := DecodeBody(compressed, encoding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, body) {
		t.Error("round trip mismatch")
	}
}

// 小 body 不压缩。
func TestSmallBodyNotCompressed(t *testing.T) {
	body, encoding := CompressBody([]byte(`{"model":"x"}`))
	if encoding != "" {
		t.Errorf("small body must not compress, got %q", encoding)
	}
	if !bytes.Equal(body, []byte(`{"model":"x"}`)) {
		t.Error("body must be unchanged")
	}
}

// 压缩失败不致命：解压坏数据报 400 而非 panic。
func TestCorruptZstdRejected(t *testing.T) {
	_, err := DecodeBody([]byte("not zstd at all"), "zstd")
	if err == nil {
		t.Fatal("corrupt zstd must fail")
	}
	if HTTPStatus(err) < 400 {
		t.Errorf("decode failure should map to 4xx, got %d", HTTPStatus(err))
	}
}

// 未知编码 415。
func TestUnknownEncoding(t *testing.T) {
	_, err := DecodeBody([]byte("x"), "br")
	if HTTPStatus(err) != http.StatusUnsupportedMediaType {
		t.Errorf("br should 415, got %d", HTTPStatus(err))
	}
}

// 重试：503 两次后成功。
func TestRetryOn503(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	opts := DefaultRetryOptions()
	opts.BackoffMs = 1
	resp, retries, err := FetchWithRetry(context.Background(), http.MethodPost, ts.URL, nil, []byte("{}"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if retries != 2 {
		t.Errorf("retries = %d, want 2", retries)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// 不重试：500 与 429 透传。
func TestNoRetryOn500And429(t *testing.T) {
	for _, status := range []int{500, 429, 400} {
		var calls int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(status)
		}))
		resp, retries, err := FetchWithRetry(context.Background(), http.MethodPost, ts.URL, nil, nil, DefaultRetryOptions())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		ts.Close()
		if retries != 0 || calls != 1 {
			t.Errorf("status %d: retries=%d calls=%d, want 0/1", status, retries, calls)
		}
	}
}

// canRetry 红线：调用方已中继字节后不再重试。
func TestCanRetryGate(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(503)
	}))
	defer ts.Close()

	opts := DefaultRetryOptions()
	opts.BackoffMs = 1
	relayClosed := false
	opts.CanRetry = func() bool { return relayClosed }
	resp, retries, err := FetchWithRetry(context.Background(), http.MethodPost, ts.URL, nil, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if retries != 0 || calls != 1 {
		t.Errorf("canRetry=false must prevent retry: retries=%d calls=%d", retries, calls)
	}
}

// 预算耗尽：昂贵的第一失败不重试。
func TestBudgetStopsRetry(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(503)
	}))
	defer ts.Close()

	opts := DefaultRetryOptions()
	opts.BudgetMs = 1 // 第一次尝试就超预算
	resp, retries, err := FetchWithRetry(context.Background(), http.MethodPost, ts.URL, nil, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if retries != 0 {
		t.Errorf("budget should stop retry, retries=%d", retries)
	}
}

// 连接拒绝（ECONNREFUSED）是可重试的传输错误。
func TestConnectRefusedRetryable(t *testing.T) {
	// 找一个保证没人监听的端口。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close()

	opts := DefaultRetryOptions()
	opts.Retries = 1
	opts.BackoffMs = 1
	_, retries, err := FetchWithRetry(context.Background(), http.MethodPost, url, nil, nil, opts)
	if err == nil {
		t.Fatal("expected connection error")
	}
	if retries != 1 {
		t.Errorf("ECONNREFUSED should retry once, retries=%d", retries)
	}
}

// ---- 空闲看门狗 ----

// 慢生产者流：字节间隔小于窗口 → 不误杀，读完拿 EOF。
func TestIdleReaderKeepsSlowStreamAlive(t *testing.T) {
	pr, pw := io.Pipe()
	wrapped := NewIdleReadCloser(pr, 150*time.Millisecond)
	go func() {
		for i := 0; i < 3; i++ {
			pw.Write([]byte("chunk"))
			time.Sleep(50 * time.Millisecond)
		}
		pw.Close()
	}()
	raw, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("slow stream must survive: %v", err)
	}
	if string(raw) != "chunkchunkchunk" {
		t.Errorf("stream content mismatch: %q", raw)
	}
}

// 挂死流：窗口内零字节 → Read 返回 ErrUpstreamIdle 链上的错误。
func TestIdleReaderTripsOnSilence(t *testing.T) {
	pr, _ := io.Pipe() // 永不写入、永不关闭
	wrapped := NewIdleReadCloser(pr, 60*time.Millisecond)
	started := time.Now()
	_, err := wrapped.Read(make([]byte, 16))
	if err == nil {
		t.Fatal("silent stream must fail")
	}
	if !errors.Is(err, ErrUpstreamIdle) {
		t.Fatalf("error must be ErrUpstreamIdle, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("watchdog must fail fast, took %v", elapsed)
	}
	// tripped 之后的 Read 持续返回同一哨兵。
	if _, err := wrapped.Read(make([]byte, 16)); !errors.Is(err, ErrUpstreamIdle) {
		t.Errorf("subsequent reads must stay typed, got %v", err)
	}
	wrapped.Close()
}

// 管道级：FetchWithRetry 接受的响应 body 挂死 → ReadAll 收到哨兵。
func TestFetchWithRetryWrapsIdle(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release // 挂死：头已发，body 永不产出
	}))
	defer ts.Close()
	defer close(release)

	opts := DefaultRetryOptions()
	opts.IdleTimeout = 80 * time.Millisecond
	resp, _, err := FetchWithRetry(context.Background(), http.MethodPost, ts.URL, nil, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, ErrUpstreamIdle) {
		t.Fatalf("stream stall must surface ErrUpstreamIdle, got %v", err)
	}
}

// IdleTimeout=0 → 不包装（看门狗关闭）。
func TestIdleReaderDisabled(t *testing.T) {
	pr, _ := io.Pipe()
	if got := NewIdleReadCloser(pr, 0); got != io.ReadCloser(pr) {
		t.Fatal("d<=0 must return the original reader")
	}
}
