package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/httpx"
	"github.com/loyd/codex-router/internal/nativebackend"
	"github.com/loyd/codex-router/internal/usage"
)

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
	normalizeLegacyCustomToolIDs(payload)

	// 凭据被替换的无会话调用方：归一到 native 端点的窄请求面。
	if !s.native.CallerHasCredential(r.Header) {
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

	resp, err := s.native.Relay(r.Context(), nativebackend.RelayRequest{
		Route: route, Body: normalized, Header: r.Header, Compress: true,
	})
	if err != nil {
		logf("native request failed model=%s error=%v", requestedModel, err)
		writeJSON(w, http.StatusBadGateway, errBody("local_router_error", "The local router could not complete the request."))
		return
	}
	defer resp.Body.Close()
	s.relayNative(w, resp)
	s.recordTurn(usage.Event{
		Model: requestedModel, Provider: "openai",
		Status: resp.Status, DurationMs: time.Since(started).Milliseconds(),
	})
	logf("model=%s provider=openai status=%d duration_ms=%d",
		requestedModel, resp.Status, time.Since(started).Milliseconds())
}

// normalizeLegacyCustomToolIDs repairs custom-tool items produced by older
// router versions. Those versions emitted a custom_tool_call with an fc_ item
// ID, but the Responses contract requires ctc_ for custom calls (and ctco_ for
// custom-tool outputs). Keeping this at the Responses request boundary lets
// an existing session recover without discarding its history.
func normalizeLegacyCustomToolIDs(payload map[string]any) bool {
	changed := normalizeLegacyCustomToolItems(payload["input"])
	if response, ok := payload["response"].(map[string]any); ok {
		changed = normalizeLegacyCustomToolItems(response["input"]) || changed
	}
	return changed
}

func normalizeLegacyCustomToolItems(raw any) bool {
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		prefix := ""
		switch item["type"] {
		case "custom_tool_call":
			prefix = "ctc_"
		case "custom_tool_call_output":
			prefix = "ctco_"
		default:
			continue
		}
		id, _ := item["id"].(string)
		if strings.HasPrefix(id, "fc_") {
			item["id"] = prefix + strings.TrimPrefix(id, "fc_")
			changed = true
		}
	}
	return changed
}

// relayNative 把 Native Backend 的受限响应适配回 HTTP transport。
func (s *Server) relayNative(w http.ResponseWriter, resp *nativebackend.RelayResponse) {
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
	w.WriteHeader(resp.Status)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logf("native relay truncated status=%d idle=%v err=%v", resp.Status,
			errors.Is(err, httpx.ErrUpstreamIdle), err)
	}
}

// callerForwardHeaders 提取白名单调用方头，并补齐上游凭据兜底。
// callerForwardHeaders 保留给 WS transport；header 细节由 Native Backend 负责。
func (s *Server) callerForwardHeaders(r *http.Request) map[string]string {
	return s.native.Headers(r.Header)
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
