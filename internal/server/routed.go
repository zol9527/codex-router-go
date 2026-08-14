package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/httpx"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/translate"
	"github.com/loyd/codex-router/internal/usage"
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
		writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retryAfter := 0
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			fmt.Sscanf(ra, "%d", &retryAfter)
		}
		s.writeUpstreamError(w, provider, model, &upstreamFailure{
			status: resp.StatusCode, bodyText: string(raw), retryAfter: retryAfter,
		})
		return
	}
	s.relayResponse(w, resp)
	s.recordTurn(usage.Event{Model: model.Slug,
		Provider: s.opt.Registry.CanonicalProviderID(provider.ID),
		Status:   resp.StatusCode, DurationMs: time.Since(started).Milliseconds()})
	logf("model=%s provider=%s status=%d duration_ms=%d",
		model.Slug, provider.ID, resp.StatusCode, time.Since(started).Milliseconds())
}

// ---- Responses → chat completions 翻译路径 ----

// attemptOutcome 是一次上游流尝试的完整结果：翻译器 + 累积的事件字节。
type attemptOutcome struct {
	translator *translate.ChatToResponsesSSE
	events     *translate.OutputBuffer
	status     int
}

// upstreamFailure 携带上游错误响应供翻译。
type upstreamFailure struct {
	status     int
	bodyText   string
	retryAfter int
}

func (e *upstreamFailure) Error() string { return fmt.Sprintf("upstream status %d", e.status) }

// streamRelay 实现空补全守卫的 hold/释放语义：
//   - prologue（created/added 事件）缓冲不写 —— 空流可整段替换；
//   - 首个"活性"事件（reasoning/content/tool-call delta）到达即建立
//     直通：缓冲 + 后续全部直写，从此不可重试；
//   - 流结束仍无任何活性 → 调用方可丢弃缓冲隐形重试。
//
// 这对齐 Node 版 EmptyCompletionGuard 的 hold 语义，同时保留流式
// （liveness 后的 delta 逐块写给 client，TTFT 不受影响）。
type streamRelay struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	buffered  []byte
	relayed   bool
	aliveSeen bool
	writeErr  error
}

// aliveEventPattern 匹配证明上游正在生成的事件类型。
var aliveEventPattern = regexp.MustCompile(
	"response\\.(?:reasoning_summary_text|output_text|function_call_arguments)\\.delta")

func (sr *streamRelay) headersWritten() bool { return sr.relayed }

// emit 处理一段翻译输出。
func (sr *streamRelay) emit(chunk []byte) {
	if sr.writeErr != nil {
		return
	}
	if sr.relayed {
		if _, err := sr.w.Write(chunk); err != nil {
			sr.writeErr = err
			return
		}
		if sr.flusher != nil {
			sr.flusher.Flush()
		}
		return
	}
	if aliveEventPattern.Match(chunk) {
		sr.aliveSeen = true
		sr.relayed = true
		sr.w.Header().Set("Content-Type", "text/event-stream")
		sr.w.Header().Set("Cache-Control", "no-cache")
		sr.w.WriteHeader(http.StatusOK)
		if _, err := sr.w.Write(sr.buffered); err != nil {
			sr.writeErr = err
			return
		}
		if _, err := sr.w.Write(chunk); err != nil {
			sr.writeErr = err
			return
		}
		if sr.flusher != nil {
			sr.flusher.Flush()
		}
		return
	}
	sr.buffered = append(sr.buffered, chunk...)
}

// finishFlushWith 用给定字节（隐形重试的完整翻译输出）提交头并写出。
// 仅在从未直通（首尝试是静默空流）时可用。
func (sr *streamRelay) finishFlushWith(payload []byte) error {
	if sr.writeErr != nil {
		return sr.writeErr
	}
	if sr.relayed {
		return fmt.Errorf("cannot replace a response head after it was sent")
	}
	sr.relayed = true
	sr.w.Header().Set("Content-Type", "text/event-stream")
	sr.w.Header().Set("Cache-Control", "no-cache")
	sr.w.WriteHeader(http.StatusOK)
	if _, err := sr.w.Write(payload); err != nil {
		return err
	}
	if sr.flusher != nil {
		sr.flusher.Flush()
	}
	return nil
}

// runChatAttempt 执行一次上游请求并增量翻译。relay 非 nil 时事件经
// 守卫直写 client（liveness 起）；relay 为 nil 时全部累积在 events 缓冲
// （隐形重试的第二次尝试用 —— 判定完再决定写不写）。
func (s *Server) runChatAttempt(ctx context.Context, target string, headers map[string]string,
	body []byte, model *registry.Model, estimate int, nsIndex *translate.NamespaceIndex,
	relay *streamRelay) (*attemptOutcome, error) {

	resp, _, err := httpx.FetchWithRetry(ctx, http.MethodPost, target, headers, body, httpx.DefaultRetryOptions())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 被动限额收割：响应头自报配额，零额外请求；持久化绝不在
	// time-to-first-byte 路径上（返回后由 usage 循环异步记录）。
	if snapshot := usage.ParseRateLimitHeaders(resp.Header, time.Now()); snapshot != nil && s.opt.RateLimits != nil {
		s.opt.RateLimits.Record(model.Provider, snapshot, time.Now())
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retryAfter := 0
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			fmt.Sscanf(ra, "%d", &retryAfter)
		}
		return &attemptOutcome{status: resp.StatusCode}, &upstreamFailure{
			status: resp.StatusCode, bodyText: string(raw), retryAfter: retryAfter,
		}
	}
	translator := translate.NewChatToResponsesSSE("", model.UpstreamModel).
		WithEstimatedInputTokens(estimate).
		WithNamespaceIndex(nsIndex, model.Slug)
	events := &translate.OutputBuffer{}
	created := translator.Created()
	events.Write(created)
	if relay != nil {
		relay.emit(created) // prologue 守卫的一部分：静默空流可整段替换
	}

	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	emit := func() {
		if len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if relay != nil {
			if chunk := translator.Feed(data); len(chunk) > 0 {
				relay.emit(chunk)
				events.Write(chunk)
			}
			return
		}
		events.Write(translator.Feed(data))
	}
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
		} else if trimmed == "" {
			emit()
		}
		if readErr != nil {
			emit()
			if !errors.Is(readErr, io.EOF) && ctx.Err() == nil {
				return &attemptOutcome{translator: translator, events: events, status: resp.StatusCode},
					fmt.Errorf("upstream stream ended before completion: %w", readErr)
			}
			return &attemptOutcome{translator: translator, events: events, status: resp.StatusCode}, nil
		}
	}
}

// serveChatTranslation：翻译请求发往 chat 上游，把上游 SSE 增量
// 重组成 Responses 事件流写给 Codex。
//
// 管线：tool-result aging（请求方向）→ 翻译 → 上游 → 空补全守卫
// （整流判定 + 同字节隐形重试一次）→ prompt-token 补零替换
// （completed 事件内，只落在显式零上）→ usage 计量。
func (s *Server) serveChatTranslation(w http.ResponseWriter, r *http.Request,
	payload map[string]any, model *registry.Model, provider *registry.Provider,
	credential string, started time.Time) {

	providerID := s.opt.Registry.CanonicalProviderID(provider.ID)
	setAging := func(stats translate.AgingStats) {
		s.agingMu.Lock()
		s.lastAging = stats
		s.agingMu.Unlock()
	}

	// 请求方向 aging：老的大工具结果换回执，最新 frontier 逐字节保留。
	aging := translate.AgingStats{}
	if input, ok := payload["input"].([]any); ok {
		aged, stats := translate.AgeToolResults(input)
		payload["input"] = aged
		aging = stats
	}
	setAging(aging)

	// codex app 工具合并：客户端只发精简 codex_app namespace，快照补全
	// deferLoading 推迟的部分，让路由模型看到与原生模型相同的工具集。
	if merged, changed := translate.MergeCodexAppTools(payload["tools"]); changed {
		payload["tools"] = merged
	}

	// namespace 拍平：协作运行时 / app 工具集 / MCP server 以 namespace
	// 形态下发，chat 上游只认普通 function —— 展开成 `<ns>__<tool>`，
	// 历史同步改名；响应方向的还原索引由同一个请求构建。
	nsIndex := (*translate.NamespaceIndex)(nil)
	if flattened := translate.FlattenNamespaceTools(payload["tools"]); flattened.Flattened {
		payload["tools"] = flattened.Tools
		if input, ok := payload["input"].([]any); ok {
			payload["input"] = translate.FlattenNamespacedHistory(input, flattened.Namespaces)
		}
		nsIndex = flattened.Index()
	}

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
	// 补零估算：3.3 字节/token 的保守（偏高）比率、1000 token 下限，
	// 不超过模型窗口（一个被答出的请求不可能超窗）。
	estimate := 0
	if est := int(float64(len(normalized))/3.3) + 1; est >= 1000 {
		estimate = est
		if model.ContextWindow > 0 && estimate > model.ContextWindow {
			estimate = model.ContextWindow
		}
	}

	headers := translate.UpstreamHeadersFrom(headerMap(r.Header), credential, Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "text/event-stream"
	target := strings.TrimSuffix(providerBaseURL(provider), "/") + "/chat/completions"

	stream, _ := chat.Body["stream"].(bool)
	if !stream {
		headers["Accept"] = "application/json"
		resp, _, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, target, headers, normalized, httpx.DefaultRetryOptions())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The API-provider forwarder could not complete the request."))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			retryAfter := 0
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				fmt.Sscanf(ra, "%d", &retryAfter)
			}
			s.writeUpstreamError(w, provider, model, &upstreamFailure{
				status: resp.StatusCode, bodyText: string(raw), retryAfter: retryAfter,
			})
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID,
				Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds()})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The upstream response could not be read."))
			return
		}
		var chatBody map[string]any
		if err := json.Unmarshal(raw, &chatBody); err != nil {
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The upstream response was not valid JSON."))
			return
		}
		translator := translate.NewChatToResponsesSSE("", model.UpstreamModel).
			WithEstimatedInputTokens(estimate).
			WithNamespaceIndex(nsIndex, model.Slug)
		response := translate.TranslateNonStreamChatWith(chatBody, translator)
		writeJSON(w, http.StatusOK, response)
		s.recordTurn(usage.Event{
			Model: model.Slug, Provider: providerID, Status: 200,
			DurationMs:  time.Since(started).Milliseconds(),
			InputTokens: translator.PromptTokens(), OutputTokens: translator.OutputTokens(),
			TotalTokens:          translator.TotalTokens(),
			EstimatedInputTokens: int64(translator.SubstitutedInputTokens()),
			ToolResultsAged:      aging.ToolResultsAged, ToolResultBytesSaved: aging.ToolResultBytesSaved,
		})
		return
	}

	// 流式 + 空补全守卫。
	relay := &streamRelay{w: w}
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		relay.flusher = flusher
	}
	first, firstErr := s.runChatAttempt(r.Context(), target, headers, normalized, model, estimate, nsIndex, relay)
	if firstErr != nil {
		var failure *upstreamFailure
		if errors.As(firstErr, &failure) {
			s.writeUpstreamError(w, provider, model, failure)
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID,
				Status: failure.status, DurationMs: time.Since(started).Milliseconds(),
				ToolResultsAged: aging.ToolResultsAged, ToolResultBytesSaved: aging.ToolResultBytesSaved})
			return
		}
		writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
		return
	}

	chosen := first
	emptyCompletion := !first.translator.HasContent()
	emptyRetried := false
	unrepairable := false
	if emptyCompletion && r.Context().Err() == nil && relay.writeErr == nil {
		if relay.headersWritten() {
			// 上游证明过活性（reasoning）后零产出：头已提交，不可替换，
			// 只能声明失败（Node 版 suppressedPrologue 的另一半）。
			unrepairable = true
			io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"empty_completion\",\"message\":\"The model streamed reasoning but produced no output. The router could not retry because the response had already started.\"}}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		} else {
			// 静默空流：同字节同头隐形重试一次（client 一无所见）。
			emptyRetried = true
			second, secondErr := s.runChatAttempt(r.Context(), target, headers, normalized, model, estimate, nsIndex, nil)
			switch {
			case secondErr != nil:
				var failure *upstreamFailure
				if errors.As(secondErr, &failure) {
					s.writeUpstreamError(w, provider, model, failure)
					s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID,
						Status: failure.status, DurationMs: time.Since(started).Milliseconds(),
						EmptyCompletion: true, EmptyCompletionRetried: true,
						ToolResultsAged: aging.ToolResultsAged, ToolResultBytesSaved: aging.ToolResultBytesSaved})
					return
				}
				writeJSON(w, http.StatusBadGateway, errBody("empty_completion_retry_failed",
					"The model returned an empty completion and the router's retry failed upstream."))
				s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 502,
					DurationMs:      time.Since(started).Milliseconds(),
					EmptyCompletion: true, EmptyCompletionRetried: true})
				return
			case second.translator.HasContent():
				chosen = second
				emptyCompletion = false
				if err := relay.finishFlushWith(second.events.Bytes()); err != nil {
					s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 0,
						DurationMs: time.Since(started).Milliseconds()})
					return
				}
			default:
				// 两次都空：写出第二份（空）流并声明失败。
				chosen = second
				if err := relay.finishFlushWith(second.events.Bytes()); err == nil {
					io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"empty_completion\",\"message\":\"The model returned an empty completion. The router retried once and the completion was empty again.\"}}}\n\n")
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
		}
	}

	s.recordTurn(usage.Event{
		Model: model.Slug, Provider: providerID, Status: 200,
		DurationMs:           time.Since(started).Milliseconds(),
		InputTokens:          chosen.translator.PromptTokens(),
		OutputTokens:         chosen.translator.OutputTokens(),
		TotalTokens:          chosen.translator.TotalTokens(),
		EstimatedInputTokens: int64(chosen.translator.SubstitutedInputTokens()),
		ToolResultsAged:      aging.ToolResultsAged, ToolResultBytesSaved: aging.ToolResultBytesSaved,
		EmptyCompletion: emptyCompletion, EmptyCompletionRetried: emptyRetried,
		StreamAborted: unrepairable && false,
	})
	if unrepairable {
		// unrepairable 的空补全在 usage 里同样以 EmptyCompletion 记录。
	}
	logf("model=%s provider=%s status=200 duration_ms=%d in=%d out=%d%s%s",
		model.Slug, provider.ID, time.Since(started).Milliseconds(),
		chosen.translator.PromptTokens(), chosen.translator.OutputTokens(),
		boolText(chosen.translator.SubstitutedInputTokens() > 0, " estimated-input=true"),
		boolText(emptyCompletion, " empty-completion=true"))
}

func boolText(cond bool, text string) string {
	if cond {
		return text
	}
	return ""
}

// writeUpstreamError 用完整错误翻译重写上游错误（配额/套餐分类）。
func (s *Server) writeUpstreamError(w http.ResponseWriter, provider *registry.Provider,
	model *registry.Model, failure *upstreamFailure) {
	if failure.retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", failure.retryAfter))
	}
	writeJSON(w, failure.status, translateProviderError(
		failure.status, failure.bodyText,
		model.DisplayName, provider.DisplayName, provider.ID, failure.retryAfter))
}

// recordTurn 是 usage 记录的统一入口（nil-safe）。
func (s *Server) recordTurn(event usage.Event) {
	if s.opt.Usage != nil {
		s.opt.Usage.Record(event)
	}
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
