package httpx

import (
	"bytes"
	"context"
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
