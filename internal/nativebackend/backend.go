// Package nativebackend 封装内置 ChatGPT/Codex 后端的协议细节：
// endpoint 选择、本机 auth.json 兜底、调用方头白名单与 /responses 调用。
// server 与 routing 只表达“发什么”，不感知 native 传输实现。
package nativebackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loyd/codex-router/internal/httpx"
	"github.com/loyd/codex-router/internal/vision"
)

// forwardHeaders 是允许进入 Native Backend 的调用方头。未知头留在
// HTTP transport 层，避免把本地路由密钥或代理实现细节转发出去。
var forwardHeaders = []string{
	"Authorization", "Chatgpt-Account-Id", "Openai-Beta", "Originator",
	"Session_Id", "Session-Id", "Thread-Id", "X-Client-Request-Id",
	"X-Codex-Beta-Features", "X-Codex-Installation-Id",
	"X-Codex-Parent-Thread-Id", "X-Codex-Turn-Metadata", "X-Codex-Turn-State",
	"X-Codex-Window-Id", "X-Oai-Attestation", "X-Openai-Subagent",
	"X-Responsesapi-Include-Timing-Metrics",
}

// Client 是 Native Backend 的操作语义接口。Relay 保留流式响应体；
// PostResponses/Describe 返回已经受限的完整结果。
type Client interface {
	Relay(ctx context.Context, request RelayRequest) (*RelayResponse, error)
	PostResponses(ctx context.Context, request Request) (Response, error)
	Describe(ctx context.Context, request DescribeRequest) (string, error)
	Headers(source http.Header) map[string]string
	CallerHasCredential(source http.Header) bool
}

// RelayRequest 是需要原样透传到 native endpoint 的请求。
type RelayRequest struct {
	Route  string
	Body   []byte
	Header http.Header
	// Compress 会按项目阈值压缩请求并补齐 native JSON/SSE 请求头。
	Compress bool
}

// RelayResponse 只暴露 relay 必需的传输事实，不暴露 http.Request。
type RelayResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// Request 是一次 native /responses 调用。MaxBytes 限制响应体，
// IdleWatchdog 控制读取挂死是否转换为明确错误。
type Request struct {
	Body         map[string]any
	Header       http.Header
	MaxBytes     int64
	IdleWatchdog bool
}

// Response 是 PostResponses 的受限结果。
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// DescribeRequest 用调用方会话请求 native 视觉模型转写图片。
type DescribeRequest struct {
	Engine   vision.Engine
	Effort   string
	Question string
	DataURL  string
	Header   http.Header
	MaxBytes int64
}

// Backend 是生产 adapter；本地密钥判定通过函数注入，避免依赖 server 状态。
type Backend struct {
	base         string
	client       *http.Client
	idle         time.Duration
	isLocalToken func(string) bool
}

// New 构造生产 Native Backend adapter。
func New(base string, client *http.Client, idle time.Duration, isLocalToken func(string) bool) *Backend {
	return &Backend{base: strings.TrimSuffix(base, "/"), client: client, idle: idle, isLocalToken: isLocalToken}
}

// Relay 发送并返回 native 流式响应，由调用方决定响应写回策略。
func (b *Backend) Relay(ctx context.Context, request RelayRequest) (*RelayResponse, error) {
	headers := b.Headers(request.Header)
	body := request.Body
	if request.Compress {
		var encoding string
		body, encoding = httpx.CompressBody(body)
		if encoding != "" {
			headers["Content-Encoding"] = encoding
		}
		headers["Content-Type"] = "application/json"
		headers["Accept"] = "text/event-stream"
		headers["Accept-Encoding"] = "identity"
	}
	resp, err := httpx.Fetch(ctx, http.MethodPost, b.target(request.Route), headers, body, b.client, b.idle)
	if err != nil {
		return nil, err
	}
	return &RelayResponse{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// PostResponses 调用 native /responses 并读取受限响应体。
func (b *Backend) PostResponses(ctx context.Context, request Request) (Response, error) {
	raw, err := json.Marshal(request.Body)
	if err != nil {
		return Response{}, err
	}
	idle := time.Duration(0)
	if request.IdleWatchdog {
		idle = b.idle
	}
	headers := b.Headers(request.Header)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "text/event-stream"
	headers["Accept-Encoding"] = "identity"
	resp, err := httpx.Fetch(ctx, http.MethodPost, b.target("/responses"), headers, raw, b.client, idle)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	limit := int64(8 << 20)
	if request.MaxBytes > 0 {
		limit = request.MaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Response{}, err
	}
	if int64(len(body)) > limit {
		return Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, errors.New("native response is too large")
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// Headers 从调用方请求提取白名单，并在需要时注入本机 Codex 会话。
func (b *Backend) Headers(source http.Header) map[string]string {
	headers := map[string]string{}
	for _, name := range forwardHeaders {
		if value := source.Get(name); value != "" {
			headers[name] = value
		}
	}
	if headers["Authorization"] == "" || b.isLocal(headers["Authorization"]) {
		if fallback := SessionHeaders(); len(fallback) > 0 {
			for k, v := range fallback {
				headers[k] = v
			}
		} else if headers["Authorization"] != "" {
			delete(headers, "Authorization")
		}
	}
	return headers
}

func (b *Backend) isLocal(header string) bool {
	return b.isLocalToken != nil && b.isLocalToken(header)
}

// IsLocalToken 暴露注入的本地密钥判定，供测试重建 adapter 时保持同一
// caller/internal key 语义。
func (b *Backend) IsLocalToken(header string) bool {
	return b.isLocal(header)
}

func (b *Backend) target(route string) string {
	return b.base + strings.TrimPrefix(route, "/v1")
}

// CallerHasCredential 报告调用方是否自带有效上游凭据。
func (b *Backend) CallerHasCredential(source http.Header) bool {
	header := source.Get("Authorization")
	if header == "" {
		return false
	}
	if token := BearerToken(header); token == "" {
		return true // 非 bearer 方案按外部凭据处理。
	}
	return !b.isLocal(header)
}

// BearerToken 提取 bearer token；非 bearer 形态返回空。
func BearerToken(header string) string {
	trimmed := strings.TrimSpace(header)
	const prefix = "bearer"
	if len(trimmed) <= len(prefix) || !strings.EqualFold(trimmed[:len(prefix)], prefix) {
		return ""
	}
	sep := trimmed[len(prefix)]
	if sep != ' ' && sep != '\t' {
		return ""
	}
	return strings.TrimSpace(trimmed[len(prefix)+1:])
}

var (
	sessionMu   sync.Mutex
	sessionAt   time.Time
	sessionData *codexAuthFile
)

// SessionHeaders 读取并短暂缓存 $CODEX_HOME/auth.json 的登录会话。
func SessionHeaders() map[string]string {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if sessionData == nil || time.Since(sessionAt) > 30*time.Second {
		if data, err := readCodexAuth(); err == nil {
			sessionData = data
		} else {
			sessionData = nil
		}
		sessionAt = time.Now()
	}
	data := sessionData
	if data == nil || data.AccessToken == "" || !tokenUsable(data) {
		return nil
	}
	headers := map[string]string{"Authorization": "Bearer " + data.AccessToken}
	if data.AccountID != "" {
		headers["Chatgpt-Account-Id"] = data.AccountID
	}
	return headers
}

type codexAuthTokens struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

type codexAuthFile struct {
	AccessToken string          `json:"OPENAI_API_KEY"`
	AccountID   string          `json:"chatgpt_account_id"`
	Tokens      codexAuthTokens `json:"tokens"`
}

func (f *codexAuthFile) normalize() {
	if f.AccessToken == "" {
		f.AccessToken = f.Tokens.AccessToken
	}
	if f.AccountID == "" {
		f.AccountID = f.Tokens.AccountID
	}
}

func tokenUsable(data *codexAuthFile) bool {
	parts := strings.Split(data.AccessToken, ".")
	if len(parts) != 3 {
		return false
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var payload struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(claims, &payload); err != nil {
		return false
	}
	return time.Now().Add(2*time.Minute).Unix() < int64(payload.Exp)
}

func readCodexAuth() (*codexAuthFile, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".codex")
	}
	raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return nil, err
	}
	var data codexAuthFile
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	data.normalize()
	return &data, nil
}
