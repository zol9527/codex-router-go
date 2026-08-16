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
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loyd/codex-router/internal/state"
	"time"

	"github.com/loyd/codex-router/internal/httpx"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/spill"
	"github.com/loyd/codex-router/internal/translate"
	"github.com/loyd/codex-router/internal/usage"
	"github.com/loyd/codex-router/internal/wire"
	_ "github.com/loyd/codex-router/internal/wire/chatcompletion"
	_ "github.com/loyd/codex-router/internal/wire/responses"
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

	routeModel := s.registry().ForSlug(requestedModel)
	if routeModel != nil && !s.opt.State.ProviderEnabled(routeModel.Provider, s.registry().CanonicalProviderID) {
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

	provider := s.registry().ProviderFor(routeModel)
	if provider == nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error",
			"Model "+routeModel.Slug+" has no provider definition."))
		return
	}
	providerID := s.registry().CanonicalProviderID(provider.ID)
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

	// compaction 分流：v1 是 /responses/compact 路径；v2 是 input 尾部
	// 的 compaction_trigger。压缩经协议抽象层走 provider 声明的协议。
	compactV1 := strings.HasSuffix(route, "/responses/compact")
	compactV2 := isCompactionV2(payload)
	if compactV1 || compactV2 {
		s.handleRoutedCompaction(w, r, payload, routeModel, provider, credential, compactV2, route, started)
		return
	}

	// 全部路由流量经协议抽象层：provider 声明协议（chat 翻译 / Responses
	// 直通 / 未来新增），server 管线（守卫、spill、namespace、计量）
	// 协议无关。
	s.serveRouted(w, r, payload, routeModel, provider, credential, started)
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
	resp, retries, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, s.nativeTarget(route), headers, upstreamBody, s.upstreamRetryOpts())
	if err != nil {
		logf("native request failed model=%s error=%v", requestedModel, err)
		writeJSON(w, http.StatusBadGateway, errBody("local_router_error", "The local router could not complete the request."))
		return
	}
	defer resp.Body.Close()
	s.relayResponse(w, resp)
	s.recordTurn(usage.Event{
		Model: requestedModel, Provider: "openai",
		Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds(), Retries: retries,
	})
	logf("model=%s provider=openai status=%d duration_ms=%d",
		requestedModel, resp.StatusCode, time.Since(started).Milliseconds())
}

// ---- Responses 直通（opencode-go-responses）----

// ---- Responses → chat completions 翻译路径 ----

// attemptOutcome 是一次上游流尝试的完整结果：翻译器 + 累积的事件字节。
type attemptOutcome struct {
	translator wire.StreamTranslator
	events     *translate.OutputBuffer
	status     int
	retries    int // FetchWithRetry 的额外尝试数（观测用）
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
	body []byte, model *registry.Model, proto wire.Protocol, opts wire.StreamOptions,
	relay *streamRelay) (*attemptOutcome, error) {

	resp, retries, err := httpx.FetchWithRetry(ctx, http.MethodPost, target, headers, body, s.upstreamRetryOpts())
	if err != nil {
		// outcome 带回 retries 供调用方记账。
		return &attemptOutcome{retries: retries}, err
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
		return &attemptOutcome{status: resp.StatusCode, retries: retries}, &upstreamFailure{
			status: resp.StatusCode, bodyText: string(raw), retryAfter: retryAfter,
		}
	}
	translator := proto.NewStreamTranslator(model, opts)
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
				// 错误链原样上抛（含 httpx.ErrUpstreamIdle 哨兵），
				// 由调用方决定 504/截断/502 的映射。
				return &attemptOutcome{translator: translator, events: events,
						status: resp.StatusCode, retries: retries},
					fmt.Errorf("upstream stream ended before completion: %w", readErr)
			}
			// EOF 而未见 [DONE] 哨兵（zai 偶发不发）：主动收尾补齐
			// response.completed —— 否则 Codex 判定 "stream closed
			// before response.completed" 整轮重试，思考型模型每轮
			// 60-90s 直接不可用（2026-08-16 01:10-01:12 实发三连重试）。
			// close() 幂等：正常路径已收尾则此处零输出。
			if closing := translator.Feed("[DONE]"); len(closing) > 0 {
				events.Write(closing)
				if relay != nil {
					relay.emit(closing)
				}
			}
			return &attemptOutcome{translator: translator, events: events,
				status: resp.StatusCode, retries: retries}, nil
		}
	}
}

// applySpill 对 payload["input"] 执行首过境确定性截断（见 internal/spill），
// 返回本轮统计（未启用/无 input = 零值）。开关与阈值逐请求读状态文件，
// 改完下一回合即生效；统计只在"实际发生截断"的回合累计落盘。
func (s *Server) applySpill(payload map[string]any) spill.Stats {
	enabled, maxBytes, _ := state.ReadToolResultSpill(s.opt.State.Dir)
	if !enabled {
		return spill.Stats{}
	}
	input, ok := payload["input"].([]any)
	if !ok {
		return spill.Stats{}
	}
	spilled, stats, err := spill.Process(input, spill.Options{
		Dir:      filepath.Join(s.opt.State.Dir, spill.DirName),
		MaxBytes: maxBytes,
	})
	if err != nil {
		// 部分条目落盘失败：失败的保留原文，其余已截断 —— 请求照常
		// 进行，只在日志里留痕（写盘恢复后回到一致的回执）。
		logf("spill: %v (failed items keep their original output this turn)", err)
	}
	payload["input"] = spilled
	if stats.ToolResultsSpilled > 0 {
		state.RecordSpillStats(s.opt.State.Dir, state.SpillStats{
			ResultsSpilled:       stats.ToolResultsSpilled,
			BytesSaved:           stats.ToolResultBytesSaved,
			EstimatedTokensSaved: stats.ToolResultBytesSaved / 4,
		})
	}
	return stats
}

// serveChatTranslation：翻译请求发往 chat 上游，把上游 SSE 增量
// 重组成 Responses 事件流写给 Codex。
//
// 管线：tool-result spill（请求方向，首过境确定性截断）→ 翻译 →
// 上游 → 空补全守卫（整流判定 + 同字节隐形重试一次）→ prompt-token
// 补零替换（completed 事件内，只落在显式零上）→ usage 计量。
func (s *Server) serveRouted(w http.ResponseWriter, r *http.Request,
	payload map[string]any, model *registry.Model, provider *registry.Provider,
	credential string, started time.Time) {

	providerID := s.registry().CanonicalProviderID(provider.ID)

	// 协作载荷解密：collab 的 encrypted_content 外部模型读不了，
	// 先换成明文（native 密文走中继，外部明文直接用，均带缓存）。
	if input, ok := payload["input"].([]any); ok {
		payload["input"] = s.normalizeRoutedAgentInput(r.Context(), input)
	}

	// 图片桥：文本模型收不到的贴图由视觉引擎代读，转录替换进回合
	//（无图 / 桥关 / 无引擎零成本直通；失败降级为 stated failure）。
	s.bridgeVision(w, r, payload, model)

	// 请求方向 spill：超阈值的工具结果落盘+回执。判定是内容的纯函数，
	// 同一内容任何请求产出逐字节相同的回执 —— 前缀缓存永不翻转
	//（旧 aging 的 frontier 滚动会在 turn 内/跨 turn 改写历史）。
	spillStats := s.applySpill(payload)

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

	proto, err := wire.ForProvider(provider)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("protocol_unavailable", err.Error()))
		return
	}
	prepared, err := proto.Prepare(payload, model)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request_error", err.Error()))
		return
	}
	normalized, err := json.Marshal(prepared.Body)
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
	headers["Accept"] = prepared.Accept
	target := strings.TrimSuffix(providerBaseURL(provider), "/") + prepared.Path

	// 直通协议：上游本来就是 Responses，字节原样转发
	//（守卫/翻译管线只服务需要翻译的协议）。
	if !proto.NeedsResponseTranslation() {
		resp, retries, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, target, headers, normalized, s.upstreamRetryOpts())
		if err != nil {
			logf("model=%s provider=%s status=502 duration_ms=%d protocol=responses err=%v",
				model.Slug, provider.ID, time.Since(started).Milliseconds(), err)
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The API-provider forwarder could not complete the request."))
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 502,
				DurationMs: time.Since(started).Milliseconds(), Retries: retries,
				ToolResultsSpilled: spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved})
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
			}, started)
			s.recordTurn(usage.Event{Model: model.Slug,
				Provider: s.registry().CanonicalProviderID(provider.ID),
				Status:   resp.StatusCode, DurationMs: time.Since(started).Milliseconds(),
				Retries:            retries,
				ToolResultsSpilled: spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved})
			return
		}
		s.relayResponse(w, resp)
		s.recordTurn(usage.Event{Model: model.Slug,
			Provider: s.registry().CanonicalProviderID(provider.ID),
			Status:   resp.StatusCode, DurationMs: time.Since(started).Milliseconds(), Retries: retries})
		logf("model=%s provider=%s protocol=responses status=%d duration_ms=%d",
			model.Slug, provider.ID, resp.StatusCode, time.Since(started).Milliseconds())
		return
	}

	if !prepared.Stream {
		resp, retries, err := httpx.FetchWithRetry(r.Context(), http.MethodPost, target, headers, normalized, s.upstreamRetryOpts())
		if err != nil {
			logf("model=%s provider=%s status=502 duration_ms=%d err=%v",
				model.Slug, provider.ID, time.Since(started).Milliseconds(), err)
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The API-provider forwarder could not complete the request."))
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 502,
				DurationMs: time.Since(started).Milliseconds(), Retries: retries,
				ToolResultsSpilled: spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved})
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
			}, started)
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID,
				Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds(),
				Retries:            retries,
				ToolResultsSpilled: spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			idle := errors.Is(err, httpx.ErrUpstreamIdle)
			status := http.StatusBadGateway
			errType, message := "provider_api_proxy_error", "The upstream response could not be read."
			if idle {
				status = http.StatusGatewayTimeout
				errType, message = "upstream_idle_timeout",
					"The upstream produced no data within the idle window while the response was being read."
			}
			logf("model=%s provider=%s status=%d duration_ms=%d upstream_idle=%v err=%v",
				model.Slug, provider.ID, status, time.Since(started).Milliseconds(), idle, err)
			writeJSON(w, status, errBody(errType, message))
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: status,
				DurationMs: time.Since(started).Milliseconds(), Retries: retries, UpstreamIdle: idle,
				ToolResultsSpilled: spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved})
			return
		}
		var chatBody map[string]any
		if err := json.Unmarshal(raw, &chatBody); err != nil {
			writeJSON(w, http.StatusBadGateway, errBody("provider_api_proxy_error",
				"The upstream response was not valid JSON."))
			return
		}
		response := proto.TranslateNonStream(chatBody, model, wire.StreamOptions{
			SessionModel: model.Slug, EstimateInput: estimate, NamespaceIndex: nsIndex,
			CustomTools: prepared.CustomTools,
		})
		writeJSON(w, http.StatusOK, response)
		// 计量从翻译后的 Responses usage 读取（协议无关形状）。
		inputTokens, outputTokens, totalTokens, substituted := usageFromResponsesJSON(response)
		s.recordTurn(usage.Event{
			Model: model.Slug, Provider: providerID, Status: 200,
			DurationMs:  time.Since(started).Milliseconds(),
			InputTokens: inputTokens, OutputTokens: outputTokens,
			TotalTokens:          totalTokens,
			Retries:              retries,
			EstimatedInputTokens: int64(substituted),
			ToolResultsSpilled:   spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved,
		})
		return
	}

	// 流式 + 空补全守卫。
	relay := &streamRelay{w: w}
	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		relay.flusher = flusher
	}
	streamOpts := wire.StreamOptions{
		SessionModel: model.Slug, EstimateInput: estimate, NamespaceIndex: nsIndex,
		CustomTools: prepared.CustomTools,
	}
	first, firstErr := s.runChatAttempt(r.Context(), target, headers, normalized, model, proto, streamOpts, relay)
	if firstErr != nil {
		var failure *upstreamFailure
		if errors.As(firstErr, &failure) {
			s.writeUpstreamError(w, provider, model, failure, started)
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID,
				Status: failure.status, DurationMs: time.Since(started).Milliseconds(),
				Retries:            attemptRetries(first),
				ToolResultsSpilled: spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved})
			return
		}
		s.failLiveStream(w, provider, model, firstErr, relay, started, usage.Event{
			Retries:              attemptRetries(first),
			ToolResultsSpilled:   spillStats.ToolResultsSpilled,
			ToolResultBytesSaved: spillStats.ToolResultBytesSaved,
		})
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
			second, secondErr := s.runChatAttempt(r.Context(), target, headers, normalized, model, proto, streamOpts, nil)
			switch {
			case secondErr != nil:
				var failure *upstreamFailure
				if errors.As(secondErr, &failure) {
					s.writeUpstreamError(w, provider, model, failure, started)
					s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID,
						Status: failure.status, DurationMs: time.Since(started).Milliseconds(),
						Retries:         attemptRetries(first) + attemptRetries(second),
						EmptyCompletion: true, EmptyCompletionRetried: true,
						ToolResultsSpilled: spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved})
					return
				}
				// 第二次尝试头必未提交（首次是静默空流），可安全回 JSON。
				idle := errors.Is(secondErr, httpx.ErrUpstreamIdle)
				status := http.StatusBadGateway
				errType, message := "empty_completion_retry_failed",
					"The model returned an empty completion and the router's retry failed upstream."
				if idle {
					status = http.StatusGatewayTimeout
					errType, message = "upstream_idle_timeout",
						"The upstream produced no data within the idle window during the router's retry."
				}
				logf("model=%s provider=%s status=%d duration_ms=%d empty_completion=true retried=true upstream_idle=%v err=%v",
					model.Slug, provider.ID, status, time.Since(started).Milliseconds(), idle, secondErr)
				writeJSON(w, status, errBody(errType, message))
				s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: status,
					DurationMs:      time.Since(started).Milliseconds(),
					Retries:         attemptRetries(first) + attemptRetries(second),
					UpstreamIdle:    idle,
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

	// 兜底写出：custom-tool-only 流全程无活性事件（custom 调用刻意不
	// 发 function_call_arguments.delta），头从未提交 —— 不补写的话
	// handler 静默返回 200 + content-length:0，Codex 判
	// "stream closed before response.completed" 整轮重试至耗尽
	//（2026-08-16 01:33-01:41 实发 5/5）。守卫语义不变：活性直通、
	// 隐形重试或失败声明的路径头均已提交，此处零输出。
	if !relay.headersWritten() && relay.writeErr == nil && r.Context().Err() == nil {
		if err := relay.finishFlushWith(chosen.events.Bytes()); err != nil {
			s.recordTurn(usage.Event{Model: model.Slug, Provider: providerID, Status: 0,
				DurationMs: time.Since(started).Milliseconds()})
			return
		}
	}

	// 重试记账：实际发生过的尝试都计入（隐形重试时 chosen 是第二次）。
	totalRetries := attemptRetries(first)
	if emptyRetried {
		totalRetries += attemptRetries(chosen)
	}
	s.recordTurn(usage.Event{
		Model: model.Slug, Provider: providerID, Status: 200,
		DurationMs:           time.Since(started).Milliseconds(),
		InputTokens:          chosen.translator.PromptTokens(),
		OutputTokens:         chosen.translator.OutputTokens(),
		TotalTokens:          chosen.translator.TotalTokens(),
		EstimatedInputTokens: int64(chosen.translator.SubstitutedInputTokens()),
		Retries:              totalRetries,
		ToolResultsSpilled:   spillStats.ToolResultsSpilled, ToolResultBytesSaved: spillStats.ToolResultBytesSaved,
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

// attemptRetries 从尝试结果安全取 FetchWithRetry 的额外尝试数。
func attemptRetries(outcome *attemptOutcome) int {
	if outcome == nil {
		return 0
	}
	return outcome.retries
}

// failLiveStream 处理传输/看门狗类失败的收尾（响应从未给过结论）：
//   - 头未提交：回 JSON 状态 —— 空闲看门狗 504（Codex 对 5xx 自带
//     重试接管），其余传输失败 502；
//   - 头已提交（liveness 后中断）：绝不能再 writeJSON —— 那会触发
//     superfluous WriteHeader 并把 JSON 追加进 SSE 流。只截断返回，
//     Codex 按既有语义整轮重试。
func (s *Server) failLiveStream(w http.ResponseWriter, provider *registry.Provider, model *registry.Model,
	err error, relay *streamRelay, started time.Time, base usage.Event) {

	idle := errors.Is(err, httpx.ErrUpstreamIdle)
	status := http.StatusBadGateway
	if idle {
		status = http.StatusGatewayTimeout
	}
	base.Model, base.Provider = model.Slug, provider.ID
	base.Status = status
	base.DurationMs = time.Since(started).Milliseconds()
	base.UpstreamIdle = idle

	if relay != nil && (relay.headersWritten() || relay.writeErr != nil) {
		base.StreamAborted = true
		logf("model=%s provider=%s status=%d duration_ms=%d stream_truncated=true upstream_idle=%v err=%v",
			model.Slug, provider.ID, status, base.DurationMs, idle, err)
		s.recordTurn(base)
		return
	}
	logf("model=%s provider=%s status=%d duration_ms=%d upstream_idle=%v err=%v",
		model.Slug, provider.ID, status, base.DurationMs, idle, err)
	if idle {
		writeJSON(w, status, errBody("upstream_idle_timeout",
			"The upstream stream produced no data within the configured idle window; the router closed it so the client can retry."))
	} else {
		writeJSON(w, status, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
	}
	s.recordTurn(base)
}

// usageFromResponsesJSON 从 Responses 形态的响应体提取计量
// （input/output/total tokens 与补零替换是否发生 —— 替换后的 input
// 与 provider 原值无法在此区分，estimated 以 usage.total -
// (input+output) 之差近似不可靠，改由翻译器在流路径精确报告；
// 非流式路径按 Responses usage 原值记录）。
func usageFromResponsesJSON(response map[string]any) (int64, int64, int64, int) {
	usageField, _ := response["usage"].(map[string]any)
	read := func(key string) int64 {
		if v, ok := usageField[key].(float64); ok {
			return int64(v)
		}
		return 0
	}
	return read("input_tokens"), read("output_tokens"), read("total_tokens"), 0
}

func boolText(cond bool, text string) string {
	if cond {
		return text
	}
	return ""
}

// writeUpstreamError 用完整错误翻译重写上游错误（配额/套餐分类）。
// 失败同样落 router.log —— 此前失败请求只进 usage-events.jsonl，
// 日志审计时会漏（2026-08-16 黑洞 500 只能靠 usage 考古发现）。
func (s *Server) writeUpstreamError(w http.ResponseWriter, provider *registry.Provider,
	model *registry.Model, failure *upstreamFailure, started time.Time) {
	logf("model=%s provider=%s status=%d duration_ms=%d upstream_error=%.200s",
		model.Slug, provider.ID, failure.status, time.Since(started).Milliseconds(), failure.bodyText)
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
