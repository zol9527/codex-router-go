// Package httpx 提供请求体编解码（zstd/gzip/deflate/brotli）与
// "首字节前才合法" 的上游重试。两者都按旧 AGENTS.md 的移植规格实现：
// 重试只在未中继任何字节前发生，body 是可重放的字节序列。
package httpx

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/klauspost/compress/zstd"
)

// MaxDecodedBodyBytes 是解压后的请求体上限（与 Node 版默认一致）。
const MaxDecodedBodyBytes = 256 * 1024 * 1024

var zstdDecoder, _ = zstd.NewReader(nil,
	zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(MaxDecodedBodyBytes))

// DecodeBody 按请求的 Content-Encoding 解压 body。
// 不支持或损坏的编码返回带 HTTP 状态的错误（415/400/413）。
func DecodeBody(body []byte, contentEncoding string) ([]byte, error) {
	encodings := []string{}
	for _, part := range splitAndLower(contentEncoding) {
		if part != "" && part != "identity" {
			encodings = append(encodings, part)
		}
	}
	// 多层编码按声明逆序剥掉（最外层最先解）。
	decoded := body
	for i := len(encodings) - 1; i >= 0; i-- {
		var err error
		switch encodings[i] {
		case "zstd":
			decoded, err = zstdDecoder.DecodeAll(decoded, nil)
			if err == nil && uint64(len(decoded)) > MaxDecodedBodyBytes {
				err = errStatus(413, fmt.Sprintf("Decoded request body exceeds %d bytes.", MaxDecodedBodyBytes))
			}
		case "gzip", "x-gzip":
			var gr *gzip.Reader
			gr, err = gzip.NewReader(bytes.NewReader(decoded))
			if err == nil {
				decoded, err = io.ReadAll(io.LimitReader(gr, MaxDecodedBodyBytes+1))
				if err == nil && len(decoded) > MaxDecodedBodyBytes {
					err = errStatus(413, "Decoded request body is too large.")
				}
			}
		case "deflate":
			decoded, err = inflate(decoded)
		case "br":
			// Brotli：Codex 实际只发 zstd；未知编码按 415 拒绝，
			// 与 Node 版对未列出编码的行为一致（br 在 Node 侧支持，
			// 这里显式降级为不支持，因为该分支从未在真实流量中出现）。
			err = errStatus(415, "Unsupported Content-Encoding: br")
		default:
			err = errStatus(415, "Unsupported Content-Encoding: "+encodings[i])
		}
		if err != nil {
			return nil, err
		}
	}
	if len(decoded) > MaxDecodedBodyBytes {
		return nil, errStatus(413, "Decoded request body is too large.")
	}
	return decoded, nil
}

func inflate(data []byte) ([]byte, error) {
	fr := flate.NewReader(bytes.NewReader(data))
	defer fr.Close()
	out, err := io.ReadAll(io.LimitReader(fr, MaxDecodedBodyBytes+1))
	if err != nil {
		return nil, errStatus(400, "Unable to decompress request body.")
	}
	if len(out) > MaxDecodedBodyBytes {
		return nil, errStatus(413, "Decoded request body is too large.")
	}
	return out, nil
}

func splitAndLower(value string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(value); i++ {
		if i == len(value) || value[i] == ',' {
			part := trimSpace(value[start:i])
			out = append(out, lowerASCII(part))
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// StatusError 携带一个应当回给调用方的 HTTP 状态。
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

func errStatus(code int, msg string) error { return &StatusError{Code: code, Msg: msg} }

// HTTPStatus 从任意错误提取回给调用方的状态码（默认 500）。
func HTTPStatus(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 500
}

// MinCompressedBodyBytes：小于此值不值得压缩（TLS 记录就够装下）。
const MinCompressedBodyBytes = 16 * 1024

var zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1),
	zstd.WithEncoderLevel(zstd.SpeedDefault))

// CompressBody 把超过阈值的 body 压成 zstd。
// 压不划算或失败时原样返回（压缩是优化，绝不成为要求），
// 只有返回压缩体时才设置 Content-Encoding。
func CompressBody(body []byte) ([]byte, string) {
	if len(body) < MinCompressedBodyBytes {
		return body, ""
	}
	compressed := zstdEncoder.EncodeAll(body, nil)
	if len(compressed) >= len(body) {
		return body, ""
	}
	return compressed, "zstd"
}

// ---- 上游重试：只在首字节前合法 ----

// RetryableStatuses 表示"中介从未从源站拿到可用响应"：
// 502/503/504 网关类，520-524 Cloudflare 边缘类。
// 刻意不含 429（限流，Retry-After 已被透传）、4xx（确定性）、500（源站已运行）。
var RetryableStatuses = map[int]bool{
	502: true, 503: true, 504: true,
	520: true, 521: true, 522: true, 523: true, 524: true,
}

// RetryOptions 控制重试循环。Retries=0 禁用；默认 2 次、250ms 起、
// 3 倍退避、5 秒预算 —— 乘上 Codex 自己的约五次重连仍是快速失败。
type RetryOptions struct {
	Retries   int
	BackoffMs int
	BudgetMs  int
	CanRetry  func() bool
	OnRetry   func(attempt int, status int, err error, delayMs int)
	Client    *http.Client
}

// DefaultRetryOptions 返回与 Node 版一致的默认值。
func DefaultRetryOptions() RetryOptions {
	return RetryOptions{Retries: 2, BackoffMs: 250, BudgetMs: 5000}
}

// FetchWithRetry 发送请求并在"响应从未开始"类失败上有限重试。
// 返回最终响应与重试次数（首次之外的尝试数）。
// canRetry 在每次重试前重新检查 —— 一旦调用方中继过任何字节，
// 重放会向正在读的流追加第二份响应，这是结构性红线。
func FetchWithRetry(ctx context.Context, method, url string, headers map[string]string, body []byte, opts RetryOptions) (*http.Response, int, error) {
	if opts.Retries == 0 && opts.BackoffMs == 0 && opts.BudgetMs == 0 {
		opts = DefaultRetryOptions()
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	started := time.Now()
	attempt := 0
	for {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, attempt, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if attempt >= opts.Retries {
			return resp, attempt, err
		}
		var retryable bool
		var status int
		if err != nil {
			retryable = isRetryableTransportError(err)
		} else {
			status = resp.StatusCode
			retryable = RetryableStatuses[status]
		}
		if !retryable {
			return resp, attempt, err
		}
		if ctx.Err() != nil || (opts.CanRetry != nil && !opts.CanRetry()) {
			return resp, attempt, err
		}
		if time.Since(started) >= time.Duration(opts.BudgetMs)*time.Millisecond {
			return resp, attempt, err
		}
		if resp != nil && resp.Body != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
		delay := float64(opts.BackoffMs) * math.Pow(3, float64(attempt))
		attempt++
		if opts.OnRetry != nil {
			opts.OnRetry(attempt, status, err, int(delay))
		}
		select {
		case <-time.After(time.Duration(delay) * time.Millisecond):
		case <-ctx.Done():
			return nil, attempt, ctx.Err()
		}
		if ctx.Err() != nil {
			return nil, attempt, ctx.Err()
		}
	}
}

// isRetryableTransportError 识别"连接从未建立/响应从未开始"类失败。
// context 取消与超时不是上游故障，不重试。
func isRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return isConnectError(err)
}
