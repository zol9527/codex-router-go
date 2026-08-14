package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/loyd/codex-router/internal/httpx"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/translate"
)

// handleResponses 是 /responses 主入口：按 model 分流。
// 命中注册表 → 翻译后发往 chat 上游或 Responses 直通；
// 未命中 → native GPT 流量直连 ChatGPT 后端。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request, route string) {
	started := time.Now()
	setRoute, finish := s.beginRequest()
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
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Invalid JSON request: "+err.Error()))
		return
	}
	requestedModel, _ := payload["model"].(string)

	routeModel := s.opt.Registry.ForSlug(requestedModel)
	if routeModel != nil && !s.opt.State.ProviderEnabled(routeModel.Provider, s.opt.Registry.CanonicalProviderID) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"type": "provider_not_enabled", "provider": routeModel.Provider,
				"message": "Provider " + routeModel.Provider + " is hidden. Enable it via codex-router control.",
			},
		})
		return
	}
	if routeModel == nil {
		// 未注册模型 = native GPT 流量。
		s.handleNativeTurn(w, r, route, payload, requestedModel, setRoute, started)
		return
	}

	provider := s.opt.Registry.ProviderFor(routeModel)
	if provider == nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error",
			"Model "+routeModel.Slug+" has no provider definition."))
		return
	}
	providerID := s.opt.Registry.CanonicalProviderID(provider.ID)
	setRoute(providerID, routeModel.Slug, sessionNameFromHeaders(r.Header))

	credential, _ := s.opt.Credentials.Resolve(provider)
	if credential == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"type": "provider_api_key_missing", "provider": provider.ID,
				"message": provider.DisplayName + " API key is not configured. Set it via codex-router control credential.",
			},
		})
		return
	}

	if provider.Protocol == "openai-responses" {
		s.serveResponsesPassthrough(w, r, payload, routeModel, provider, credential, started)
		return
	}
	s.serveChatTranslation(w, r, payload, routeModel, provider, credential, started)
}

// handleNativeTurn：未命中注册表的模型按 native 流量直连。
func (s *Server) handleNativeTurn(w http.ResponseWriter, r *http.Request, route string,
	payload map[string]any, requestedModel string, setRoute func(string, string, string), started time.Time) {

	setRoute("openai", requestedModel, sessionNameFromHeaders(r.Header))
	native := payload
	if s.callerBroughtNoUpstreamCredential(r) {
		native = cloneMap(payload)
		native["store"] = false
		for _, key := range nativeUnsupportedParams {
			delete(native, key)
		}
	}
	normalized, err := json.Marshal(native)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Unable to encode request."))
		return
	}
	headers := s.nativeHeaders(r)
	upstreamBody, encoding := httpx.CompressBody(normalized)
	if encoding != "" {
		headers["Content-Encoding"] = encoding
	}
	resp, _, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, s.nativeTarget(route), headers, upstreamBody, httpx.DefaultRetryOptions())
	if err != nil {
		logf("native request failed model=%s error=%v", requestedModel, err)
		writeJSON(w, http.StatusBadGateway, errBody("local_router_error", "The local router could not complete the request."))
		return
	}
	defer resp.Body.Close()
	s.relayResponse(w, resp)
	logf("model=%s provider=openai status=%d duration_ms=%d",
		requestedModel, resp.StatusCode, time.Since(started).Milliseconds())
}

// ---- Responses 直通（opencode-go-responses）----

// serveResponsesPassthrough：上游原生说 Responses，只需替换 model、
// 注入凭据、剥掉路由标记头，协议本身不动。
func (s *Server) serveResponsesPassthrough(w http.ResponseWriter, r *http.Request,
	payload map[string]any, model *registry.Model, provider *registry.Provider,
	credential string, started time.Time) {

	translated := cloneMap(payload)
	translated["model"] = model.UpstreamModel
	delete(translated, "client_metadata")
	// Responses 端点形态：store/include 等 Codex 字段上游认得，保留。
	normalized, err := json.Marshal(translated)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Unable to encode request."))
		return
	}
	headers := translate.UpstreamHeadersFrom(headerMap(r.Header), credential, Version)
	headers["Content-Type"] = "application/json"
	if stream, ok := translated["stream"].(bool); ok && stream {
		headers["Accept"] = "text/event-stream"
	}
	target := strings.TrimSuffix(providerBaseURL(provider), "/") + "/responses"
	resp, _, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, target, headers, normalized, httpx.DefaultRetryOptions())
	if err != nil {
		s.relayUpstreamFailure(w, provider, model, resp, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		s.relayUpstreamFailure(w, provider, model, resp, nil)
		return
	}
	s.relayResponse(w, resp)
	logf("model=%s provider=%s status=%d duration_ms=%d",
		model.Slug, provider.ID, resp.StatusCode, time.Since(started).Milliseconds())
}

// ---- Responses → chat completions 翻译路径 ----

// serveChatTranslation：翻译请求发往 chat 上游，把上游 SSE 增量
// 重组成 Responses 事件流写给 Codex。
func (s *Server) serveChatTranslation(w http.ResponseWriter, r *http.Request,
	payload map[string]any, model *registry.Model, provider *registry.Provider,
	credential string, started time.Time) {

	chat, err := translate.TranslateToChat(payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", err.Error()))
		return
	}
	translate.ApplyRequestProfile(chat.Body, chat.RequestedEffort, model)
	chat.Body["model"] = model.UpstreamModel

	normalized, err := json.Marshal(chat.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", "Unable to encode request."))
		return
	}
	headers := translate.UpstreamHeadersFrom(headerMap(r.Header), credential, Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "text/event-stream"

	target := strings.TrimSuffix(providerBaseURL(provider), "/") + "/chat/completions"
	resp, _, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, target, headers, normalized, httpx.DefaultRetryOptions())
	if err != nil {
		s.relayUpstreamFailure(w, provider, model, resp, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		s.relayUpstreamFailure(w, provider, model, resp, nil)
		return
	}

	stream, _ := chat.Body["stream"].(bool)
	if !stream {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The API-provider forwarder could not complete the request."))
			return
		}
		var chatBody map[string]any
		if err := json.Unmarshal(raw, &chatBody); err != nil {
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The upstream response was not valid JSON."))
			return
		}
		response := translate.TranslateNonStreamChat(chatBody, "", model.UpstreamModel)
		writeJSON(w, http.StatusOK, response)
		return
	}
	s.streamTranslatedSSE(w, r, resp, model, provider, started)
}

// streamTranslatedSSE 读取上游 chat SSE，逐块翻译成 Responses 事件写回。
func (s *Server) streamTranslatedSSE(w http.ResponseWriter, r *http.Request,
	resp *http.Response, model *registry.Model, provider *registry.Provider, started time.Time) {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var firstToken int64
	translator := translate.NewChatToResponsesSSE("", model.UpstreamModel)
	if _, err := w.Write(translator.Created()); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}

	// SSE 读循环：按空行分块，收集 data: 行。
	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	flushBlock := func() {
		if len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if out := translator.Feed(data); len(out) > 0 {
			if atomic.LoadInt64(&firstToken) == 0 && strings.Contains(string(out), ".delta") {
				atomic.StoreInt64(&firstToken, time.Since(started).Milliseconds())
			}
			if _, err := w.Write(out); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			value := strings.TrimPrefix(trimmed, "data:")
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		} else if trimmed == "" {
			flushBlock()
		}
		if err != nil {
			flushBlock()
			// 流异常中断：写一个协议错误事件并结束（半份响应比
			// 拼接两份响应安全；与"部分流即失败"的旧规则一致）。
			if !errors.Is(err, io.EOF) && r.Context().Err() == nil {
				io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"upstream_disconnected\",\"message\":\"The upstream stream ended before completion.\"}}}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			logf("model=%s provider=%s status=200 stream=completed duration_ms=%d",
				model.Slug, provider.ID, time.Since(started).Milliseconds())
			return
		}
	}
}

// relayUpstreamFailure 把上游错误重写成命名 provider 的错误
// （错误翻译完整版在 M3；这里保证 provider 名可读、不泄内部链）。
func (s *Server) relayUpstreamFailure(w http.ResponseWriter, provider *registry.Provider,
	model *registry.Model, resp *http.Response, err error) {
	status := http.StatusBadGateway
	if err == nil && resp != nil {
		status = resp.StatusCode
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
	}
	message := "The upstream provider failed to answer the request."
	if err != nil {
		logf("upstream transport failed provider=%s error=%v", provider.ID, err)
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":     "provider_api_error",
			"code":     "provider_api_error",
			"provider": provider.DisplayName,
			"message":  model.DisplayName + " (via " + provider.DisplayName + ") failed: " + message,
		},
	})
}

// providerBaseURL 解析 provider 的上游地址（env 覆盖 > 注册表默认）。
func providerBaseURL(p *registry.Provider) string {
	if p.BaseURLEnv != "" {
		if v := os.Getenv(p.BaseURLEnv); v != "" {
			return v
		}
	}
	return p.BaseURL
}

func headerMap(header http.Header) map[string][]string { return header }

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

var _ = bytes.MinRead
