package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loyd/codex-router/internal/httpx"
	"github.com/loyd/codex-router/internal/usage"
)

var base64RawURL = base64.RawURLEncoding

// forwardHeaders 是允许透传到 native 后端的调用方头集合
// （会话凭据与 Codex 协同元数据）。其余头一概不转发。
var forwardHeaders = []string{
	"Authorization", "Chatgpt-Account-Id", "Openai-Beta", "Originator",
	"Session_Id", "Session-Id", "Thread-Id", "X-Client-Request-Id",
	"X-Codex-Beta-Features", "X-Codex-Installation-Id",
	"X-Codex-Parent-Thread-Id", "X-Codex-Turn-Metadata", "X-Codex-Turn-State",
	"X-Codex-Window-Id", "X-Oai-Attestation", "X-Openai-Subagent",
	"X-Responsesapi-Include-Timing-Metrics",
}

// nativeUnsupportedParams 是 ChatGPT 后端拒绝的公共 Responses 参数。
// 仅对"凭据被替换的调用方"归一（Codex 自己的请求本来就合规）。
var nativeUnsupportedParams = []string{
	"temperature", "top_p", "presence_penalty", "frequency_penalty",
	"max_tokens", "max_output_tokens", "metadata", "seed", "user", "truncation",
}

// handleNative 把请求原样转发到 native ChatGPT 后端。
// native 流量不重写请求体（除压缩），不翻译响应 —— 与 Node 版一致。
func (s *Server) handleNative(w http.ResponseWriter, r *http.Request, route string, defaultModel string) {
	started := time.Now()
	setRoute, finish := s.beginRequest()
	requestedModel := defaultModel
	defer func() { finish(wStatus(w)) }()

	if !requireCodexTransport(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, httpx.MaxDecodedBodyBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Unable to read request body."))
		return
	}
	decoded, err := httpx.DecodeBody(body, r.Header.Get("Content-Encoding"))
	if err != nil {
		writeJSON(w, httpx.HTTPStatus(err), errBody("invalid_request_error", err.Error()))
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Request JSON must be an object."))
		return
	}
	if m, ok := payload["model"].(string); ok && m != "" {
		requestedModel = m
	}
	setRoute("openai", requestedModel, sessionNameFromHeaders(r.Header))

	// 凭据被替换的无会话调用方：归一到 native 端点的窄请求面。
	if s.callerBroughtNoUpstreamCredential(r) {
		payload["store"] = false
		for _, key := range nativeUnsupportedParams {
			delete(payload, key)
		}
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Unable to encode request."))
		return
	}

	headers := s.nativeHeaders(r)
	upstreamBody, encoding := httpx.CompressBody(normalized)
	if encoding != "" {
		headers["Content-Encoding"] = encoding
	}

	target := s.nativeTarget(route)
	resp, _, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, target, headers, upstreamBody, httpx.DefaultRetryOptions())
	if err != nil {
		logf("native request failed model=%s error=%v", requestedModel, err)
		writeJSON(w, http.StatusBadGateway, errBody("local_router_error", "The local router could not complete the request."))
		return
	}
	defer resp.Body.Close()
	s.relayResponse(w, resp)
	s.recordTurn(usage.Event{
		Model: requestedModel, Provider: "openai",
		Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds(),
	})
	logf("model=%s provider=openai status=%d duration_ms=%d",
		requestedModel, resp.StatusCode, time.Since(started).Milliseconds())
}

// relayResponse 把上游响应头与字节流转发给调用方。
// content-length/transfer-encoding/connection 由 Go 的 ResponseWriter
// 自己管理；上游若压缩过则原样透传 content-encoding 与字节。
func (s *Server) relayResponse(w http.ResponseWriter, resp *http.Response) {
	skip := map[string]bool{
		"content-length": true, "transfer-encoding": true,
		"connection": true, "keep-alive": true,
	}
	for name, values := range resp.Header {
		if skip[strings.ToLower(name)] {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// nativeHeaders 从调用方请求里提取白名单头并补充兜底凭据。
func (s *Server) nativeHeaders(r *http.Request) map[string]string {
	headers := map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "text/event-stream",
		"Accept-Encoding": "identity",
	}
	for _, name := range forwardHeaders {
		if value := r.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	// 自带上游凭据的调用方原样透传；无凭据（或只带了本路由自己的
	// caller/internal key）时尝试注入本机 Codex 登录会话 —— 路由密钥
	// 绝不离开本机。
	if headers["Authorization"] == "" || s.isRouterLocalToken(headers["Authorization"]) {
		if fallback := s.nativeSessionHeaders(); len(fallback) > 0 {
			for k, v := range fallback {
				headers[k] = v
			}
		} else if headers["Authorization"] != "" {
			delete(headers, "Authorization")
		}
	}
	return headers
}

// isRouterLocalToken 判断 bearer token 是否本路由自己的密钥。
func (s *Server) isRouterLocalToken(header string) bool {
	token := bearerToken(header)
	if token == "" {
		return false
	}
	internal := s.opt.State.InternalKey()
	if subtleEqual(token, s.callerKey) {
		return true
	}
	return internal != "" && subtleEqual(token, internal)
}

// callerBroughtNoUpstreamCredential：调用方没有携带（有效）上游凭据。
func (s *Server) callerBroughtNoUpstreamCredential(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if header == "" {
		return true
	}
	token := bearerToken(header)
	if token == "" {
		return false // 非 bearer 方案原样透传，不算"无凭据"
	}
	return s.isRouterLocalToken(header)
}

func bearerToken(header string) string {
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

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// nativeTarget 剥掉 /v1 前缀并拼出 native 端点 URL。
func (s *Server) nativeTarget(route string) string {
	withoutV1 := strings.TrimPrefix(route, "/v1")
	return strings.TrimSuffix(s.opt.NativeBase, "/") + withoutV1
}

// ---- 本机 Codex 登录会话兜底 ----

type codexAuthFile struct {
	AccessToken string  `json:"OPENAI_API_KEY"`
	AccountID   string  `json:"chatgpt_account_id"`
	LastRefresh float64 `json:"last_refresh"`
}

var (
	nativeSessionMu   sync.Mutex
	nativeSessionAt   time.Time
	nativeSessionData *codexAuthFile
)

// nativeSessionHeaders 读取 $CODEX_HOME/auth.json 的 access token 与
// account id，提前 2 分钟过期（与 codex-native-session.mjs 一致）。
// 读取失败返回 nil —— 没有 native 会话时 native 引擎/注入不可用。
func (s *Server) nativeSessionHeaders() map[string]string {
	nativeSessionMu.Lock()
	defer nativeSessionMu.Unlock()
	if nativeSessionData == nil || time.Since(nativeSessionAt) > 30*time.Second {
		if data, err := readCodexAuth(); err == nil {
			nativeSessionData = data
		} else {
			nativeSessionData = nil
		}
		nativeSessionAt = time.Now()
	}
	data := nativeSessionData
	if data == nil || data.AccessToken == "" {
		return nil
	}
	if !codexTokenUsable(data) {
		return nil
	}
	headers := map[string]string{"Authorization": "Bearer " + data.AccessToken}
	if data.AccountID != "" {
		headers["Chatgpt-Account-Id"] = data.AccountID
	}
	return headers
}

// codexTokenUsable 解析 JWT exp 声明并提前两分钟判定过期。
// 无效载荷按不可用处理（fail closed）。
func codexTokenUsable(data *codexAuthFile) bool {
	parts := strings.Split(data.AccessToken, ".")
	if len(parts) != 3 {
		return false
	}
	claims, err := base64DecodeURL(parts[1])
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

func base64DecodeURL(value string) ([]byte, error) {
	return base64RawURL.DecodeString(value)
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
	return &data, nil
}

// wStatus 从 ResponseWriter 猜测已提交的状态（activity 收尾用）。
func wStatus(w http.ResponseWriter) int {
	type statusHinter interface{ Status() int }
	if h, ok := w.(statusHinter); ok {
		return h.Status()
	}
	return 200
}

func errBody(errType, message string) map[string]any {
	return map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	}
}
