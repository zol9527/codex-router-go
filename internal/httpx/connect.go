package httpx

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
)

// isConnectError 判断错误链上是否挂着"连接从未完成"的信号。
// 与 Node 版 RETRYABLE_ERROR_CODES 对应：ECONNREFUSED、ECONNRESET、
// ETIMEDOUT、EHOSTUNREACH、ENETDOWN/ENETUNREACH、EADDRNOTAVAIL、
// ENOBUFS、ENOTFOUND、EPIPE、EAI_AGAIN（DNS 临时失败）。
func isConnectError(err error) bool {
	for err != nil {
		if isRetryableSyscall(err) || isRetryableNet(err) || isRetryableURL(err) {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func isRetryableSyscall(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ETIMEDOUT,
			syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ENETUNREACH,
			syscall.EADDRNOTAVAIL, syscall.ENOBUFS, syscall.EPIPE:
			return true
		}
	}
	return false
}

func isRetryableNet(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// IsNotFound 且 IsTemporary 都是可重试的解析失败；
		// Go 的 EAI_AGAIN 表现为 IsTemporary。
		return dnsErr.IsTemporary || dnsErr.IsNotFound
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" || opErr.Op == "read" {
			return isConnectError(opErr.Err)
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

func isRetryableURL(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Err == os.ErrDeadlineExceeded {
			return true
		}
		// Go 1.20+ 的 http.Transport 连接错误带 "connection refused" 等文案，
		// 但那些都已被 syscall 分支覆盖；这里只兜底明确的 reset 文案。
		return strings.Contains(urlErr.Err.Error(), "connection reset")
	}
	return false
}
