// Package httpx 提供请求体编解码（zstd/gzip/deflate/brotli）与单次上游
// 请求。重试由 Codex 调用方决定；Router 只把本次上游的结果或错误返回。
package httpx

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
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

// Fetch 只发送一次上游请求。响应体可选挂空闲看门狗，防止已建立连接后
// 永久无字节；它只产生明确错误，不在 Router 内重放请求。
func Fetch(ctx context.Context, method, url string, headers map[string]string, body []byte, client *http.Client, idleTimeout time.Duration) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil || resp == nil || resp.Body == nil || idleTimeout <= 0 {
		return resp, err
	}
	resp.Body = NewIdleReadCloser(resp.Body, idleTimeout)
	return resp, nil
}
