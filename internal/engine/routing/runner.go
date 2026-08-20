package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/translate"
	"github.com/loyd/codex-router/internal/domain/usage"
	"github.com/loyd/codex-router/internal/domain/wire"
	"github.com/loyd/codex-router/internal/lib/httpx"
	"github.com/loyd/codex-router/internal/lib/logx"
)

const compactPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another language model that will resume the task.

Include current progress, key decisions, constraints, user preferences, remaining steps, and critical data or references. Be concise, structured, and focused on seamless continuation.`

// CompactionResult 是 provider 已完成的压缩摘要；最终 v1/v2 外形由
// transport 层根据原始请求选择，避免 routing 依赖 HTTP 路由细节。
type CompactionResult struct {
	Summary          string
	PromptTokens     int64
	CompletionTokens int64
}

// Run 执行一次完整的 Routing Turn：预处理输入、适配工具与 namespace、
// 调用 provider 协议，并把最终 Responses 结果写入 Sink。HTTP server 只
// 负责认证、依赖装配和将 ResponseWriter 适配为 Sink。
func (r *Runner) Run(request Request) {
	if request.Sink == nil || request.Provider == nil || request.Model == nil {
		return
	}
	if request.Context == nil {
		request.Context = context.Background()
	}
	started := request.Started
	if started.IsZero() {
		started = time.Now()
	}
	providerID := r.canonicalProviderID(request.Provider)

	if input, ok := request.Payload["input"].([]any); ok && r.NormalizeInput != nil {
		request.Payload["input"] = r.NormalizeInput(request.Context, input)
	}
	if r.BridgeVision != nil {
		r.BridgeVision(request.Context, request.Header, request.Payload, request.Model)
	}

	if merged, changed := translate.MergeCodexAppTools(request.Payload["tools"]); changed {
		request.Payload["tools"] = merged
	}
	// namespace 工具对 chat 上游必须拍平；响应侧用同一个索引还原。
	nsIndex := (*translate.NamespaceIndex)(nil)
	if flattened := translate.FlattenNamespaceTools(request.Payload["tools"]); flattened.Flattened {
		request.Payload["tools"] = flattened.Tools
		if input, ok := request.Payload["input"].([]any); ok {
			request.Payload["input"] = translate.FlattenNamespacedHistory(input, flattened.Namespaces)
		}
		nsIndex = flattened.Index()
	}

	proto, err := wire.ForProvider(request.Provider)
	if err != nil {
		request.Sink.WriteJSON(http.StatusInternalServerError, errBody("protocol_unavailable", err.Error()))
		return
	}
	// 直通协议响应字节原样转发；翻译协议必须实现 ResponseTranslator
	//（注册期契约，违反即显式失败，不静默降级）。
	var translator wire.ResponseTranslator
	if proto.NeedsResponseTranslation() {
		translator, err = wire.TranslatorFor(proto)
		if err != nil {
			request.Sink.WriteJSON(http.StatusInternalServerError, errBody("protocol_unavailable", err.Error()))
			return
		}
	}
	prepared, err := proto.Prepare(request.Payload, request.Model)
	if err != nil {
		request.Sink.WriteJSON(http.StatusBadRequest, errBody("invalid_request_error", err.Error()))
		return
	}
	if r.LogTranslationDegraded != nil {
		r.LogTranslationDegraded(prepared, request.Model)
	}
	normalized, err := json.Marshal(prepared.Body)
	if err != nil {
		request.Sink.WriteJSON(http.StatusBadRequest, errBody("invalid_request_error", "Unable to encode request."))
		return
	}
	estimate := estimateInputTokens(len(normalized), request.Model.ContextWindow)
	headers := translate.UpstreamHeadersFrom(request.Header, request.Credential, r.Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = prepared.Accept
	target := strings.TrimSuffix(r.providerBaseURL(request.Provider), "/") + prepared.Path

	if !proto.NeedsResponseTranslation() {
		r.runDirect(request, target, headers, normalized, providerID, started)
		return
	}
	if !prepared.Stream {
		r.runNonStream(request, target, headers, normalized, translator, prepared, nsIndex, estimate, providerID, started)
		return
	}
	r.runStream(request, target, headers, normalized, translator, prepared, nsIndex, estimate, providerID, started)
}

// RunCompaction 执行一次非流式 compaction provider 回合。
func (r *Runner) RunCompaction(request Request) (CompactionResult, bool) {
	if request.Sink == nil || request.Provider == nil || request.Model == nil {
		return CompactionResult{}, false
	}
	if request.Context == nil {
		request.Context = context.Background()
	}
	started := request.Started
	if started.IsZero() {
		started = time.Now()
	}
	providerID := r.canonicalProviderID(request.Provider)
	compactionPayload := cloneMap(request.Payload)
	if input, ok := compactionPayload["input"].([]any); ok && r.NormalizeInput != nil {
		compactionPayload["input"] = r.NormalizeInput(request.Context, input)
	}
	if r.BridgeVision != nil {
		r.BridgeVision(request.Context, request.Header, compactionPayload, request.Model)
	}
	if input, ok := compactionPayload["input"].([]any); ok {
		filtered := make([]any, 0, len(input)+1)
		for _, raw := range input {
			if item, ok := raw.(map[string]any); ok && item["type"] == "compaction_trigger" {
				continue
			}
			filtered = append(filtered, raw)
		}
		compactionPayload["input"] = append(filtered, compactionUserMessage(compactPrompt))
	} else {
		compactionPayload["input"] = []any{compactionUserMessage(compactPrompt)}
	}
	compactionPayload["model"] = request.Model.UpstreamModel
	compactionPayload["stream"] = false
	compactionPayload["tools"] = []any{}
	delete(compactionPayload, "previous_response_id")
	delete(compactionPayload, "client_metadata")

	proto, err := wire.ForProvider(request.Provider)
	if err != nil {
		request.Sink.WriteJSON(http.StatusInternalServerError, errBody("protocol_unavailable", err.Error()))
		return CompactionResult{}, false
	}
	prepared, err := proto.Prepare(compactionPayload, request.Model)
	if err != nil {
		request.Sink.WriteJSON(http.StatusBadRequest, errBody("invalid_request_error", err.Error()))
		return CompactionResult{}, false
	}
	if r.LogTranslationDegraded != nil {
		r.LogTranslationDegraded(prepared, request.Model)
	}
	prepared.Stream = false
	prepared.Accept = "application/json"
	normalized, err := json.Marshal(prepared.Body)
	if err != nil {
		request.Sink.WriteJSON(http.StatusBadRequest, errBody("invalid_request_error", "Unable to encode request."))
		return CompactionResult{}, false
	}
	headers := translate.UpstreamHeadersFrom(request.Header, request.Credential, r.Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = prepared.Accept
	target := strings.TrimSuffix(r.providerBaseURL(request.Provider), "/") + prepared.Path
	resp, err := httpx.Fetch(request.Context, http.MethodPost, target, headers, normalized, r.client(), r.Idle)
	if err != nil {
		request.Sink.WriteJSON(http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: 502,
			DurationMs: time.Since(started).Milliseconds()})
		return CompactionResult{}, false
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if readErr != nil {
		idle := errors.Is(readErr, httpx.ErrUpstreamIdle)
		status := http.StatusBadGateway
		errType, message := "provider_api_proxy_error", "The compaction response could not be read."
		if idle {
			status = http.StatusGatewayTimeout
			errType, message = "upstream_idle_timeout", "The upstream produced no data within the idle window while the compaction response was being read."
		}
		request.Sink.WriteJSON(status, errBody(errType, message))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: status,
			DurationMs: time.Since(started).Milliseconds(), UpstreamIdle: idle})
		return CompactionResult{}, false
	}
	if int64(len(raw)) > 32<<20 {
		request.Sink.WriteJSON(http.StatusBadGateway, errBody("provider_api_proxy_error", "Compact response is too large."))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: http.StatusBadGateway,
			DurationMs: time.Since(started).Milliseconds()})
		return CompactionResult{}, false
	}
	if resp.StatusCode >= 400 {
		r.writeUpstreamError(request, &UpstreamFailure{Status: resp.StatusCode,
			BodyText: string(raw), RetryAfter: retryAfter(resp.Header)}, started)
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
			Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds()})
		return CompactionResult{}, false
	}
	var upstreamBody map[string]any
	if err := json.Unmarshal(raw, &upstreamBody); err != nil {
		request.Sink.WriteJSON(http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The upstream compaction response was not valid JSON."))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: http.StatusBadGateway,
			DurationMs: time.Since(started).Milliseconds()})
		return CompactionResult{}, false
	}
	// 直通协议的 compaction 响应本来就是 Responses 形状，直接提取；
	// 翻译协议才需要转回。（旧实现无条件调翻译方法，直通 provider
	// 一配即 panic —— 接口拆分后这里显式分流。）
	response := upstreamBody
	if proto.NeedsResponseTranslation() {
		translator, terr := wire.TranslatorFor(proto)
		if terr != nil {
			request.Sink.WriteJSON(http.StatusInternalServerError, errBody("protocol_unavailable", terr.Error()))
			return CompactionResult{}, false
		}
		response = translator.TranslateNonStream(upstreamBody, request.Model, wire.StreamOptions{})
	}
	result := CompactionResult{Summary: extractCompactionSummary(response)}
	result.PromptTokens, result.CompletionTokens = extractCompactionUsage(upstreamBody)
	return result, true
}

func cloneMap(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func compactionUserMessage(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func extractCompactionSummary(response map[string]any) string {
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, partRaw := range content {
			if part, ok := partRaw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func extractCompactionUsage(body map[string]any) (int64, int64) {
	usageField, _ := body["usage"].(map[string]any)
	read := func(keys ...string) int64 {
		for _, key := range keys {
			if value, ok := usageField[key].(float64); ok {
				return int64(value)
			}
		}
		return 0
	}
	return read("prompt_tokens", "input_tokens"), read("completion_tokens", "output_tokens")
}

func estimateInputTokens(bodySize, contextWindow int) int {
	estimate := 0
	if value := int(float64(bodySize)/3.3) + 1; value >= 1000 {
		estimate = value
		if contextWindow > 0 && estimate > contextWindow {
			estimate = contextWindow
		}
	}
	return estimate
}

func (r *Runner) providerBaseURL(provider *registry.Provider) string {
	if r.ProviderBaseURL != nil {
		return r.ProviderBaseURL(provider)
	}
	return r.baseURL(provider)
}

func (r *Runner) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return http.DefaultClient
}

// logInfo/logError 打请求作用域日志：request.Log 预置了 req 键（与
// /activity 及 usage-events 的 requestId 同源），nil 时落回进程默认
// （测试语境不注入也不炸）。
func (r *Runner) logInfo(request Request, msg string, kv ...any) {
	r.logAt(request, slog.LevelInfo, msg, kv...)
}

func (r *Runner) logWarn(request Request, msg string, kv ...any) {
	r.logAt(request, slog.LevelWarn, msg, kv...)
}

func (r *Runner) logError(request Request, msg string, kv ...any) {
	r.logAt(request, slog.LevelError, msg, kv...)
}

func (r *Runner) logAt(request Request, level slog.Level, msg string, kv ...any) {
	logger := request.Log
	if logger == nil {
		logger = logx.Default()
	}
	logger.Log(context.Background(), level, msg, kv...)
}

func (r *Runner) runDirect(request Request, target string, headers map[string]string,
	body []byte, providerID string, started time.Time) {
	resp, err := httpx.Fetch(request.Context, http.MethodPost, target, headers, body, r.client(), r.Idle)
	if err != nil {
		r.logError(request, "request failed", "model", request.Model.Slug, "provider", request.Provider.ID,
			"status", 502, "duration_ms", time.Since(started).Milliseconds(), "protocol", "responses", "error", err)
		request.Sink.WriteJSON(http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: 502,
			DurationMs: time.Since(started).Milliseconds()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		r.writeUpstreamError(request, &UpstreamFailure{Status: resp.StatusCode,
			BodyText: readBody(resp.Body, 1<<20), RetryAfter: retryAfter(resp.Header)}, started)
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
			Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds()})
		return
	}
	r.relayResponse(request, request.Sink, resp)
	r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
		Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds()})
	r.logInfo(request, "request done", "model", request.Model.Slug, "provider", request.Provider.ID,
		"protocol", "responses", "status", resp.StatusCode, "duration_ms", time.Since(started).Milliseconds())
}

func (r *Runner) runNonStream(request Request, target string, headers map[string]string,
	body []byte, translator wire.ResponseTranslator, prepared *wire.Request, nsIndex *translate.NamespaceIndex,
	estimate int, providerID string, started time.Time) {
	resp, err := httpx.Fetch(request.Context, http.MethodPost, target, headers, body, r.client(), r.Idle)
	if err != nil {
		r.logError(request, "request failed", "model", request.Model.Slug, "provider", request.Provider.ID,
			"status", 502, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		request.Sink.WriteJSON(http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: 502,
			DurationMs: time.Since(started).Milliseconds()})
		return
	}
	defer resp.Body.Close()
	raw, readErr, tooLarge := readLimited(resp.Body, 8<<20)
	if readErr != nil {
		idle := errors.Is(readErr, httpx.ErrUpstreamIdle)
		status := http.StatusBadGateway
		errType, message := "provider_api_proxy_error", "The upstream response could not be read."
		if idle {
			status = http.StatusGatewayTimeout
			errType, message = "upstream_idle_timeout", "The upstream produced no data within the idle window while the response was being read."
		}
		r.logError(request, "upstream read failed", "model", request.Model.Slug, "provider", request.Provider.ID,
			"status", status, "duration_ms", time.Since(started).Milliseconds(), "upstream_idle", idle, "error", readErr)
		request.Sink.WriteJSON(status, errBody(errType, message))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: status,
			DurationMs: time.Since(started).Milliseconds(), UpstreamIdle: idle})
		return
	}
	if resp.StatusCode >= 400 {
		r.writeUpstreamError(request, &UpstreamFailure{Status: resp.StatusCode,
			BodyText: string(raw), RetryAfter: retryAfter(resp.Header)}, started)
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
			Status: resp.StatusCode, DurationMs: time.Since(started).Milliseconds()})
		return
	}
	if tooLarge {
		request.Sink.WriteJSON(http.StatusBadGateway, errBody("provider_api_proxy_error", "The upstream response is too large."))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: http.StatusBadGateway,
			DurationMs: time.Since(started).Milliseconds()})
		return
	}
	if err := request.Context.Err(); err != nil {
		return
	}
	var upstreamBody map[string]any
	if err := json.Unmarshal(raw, &upstreamBody); err != nil {
		request.Sink.WriteJSON(http.StatusBadGateway, errBody("provider_api_proxy_error",
			"The upstream response was not valid JSON."))
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: http.StatusBadGateway,
			DurationMs: time.Since(started).Milliseconds()})
		return
	}
	response := translator.TranslateNonStream(upstreamBody, request.Model, wire.StreamOptions{
		SessionModel: request.Model.Slug, EstimateInput: estimate,
		NamespaceIndex: nsIndex, CustomTools: prepared.CustomTools,
	})
	request.Sink.WriteJSON(http.StatusOK, response)
	inputTokens, outputTokens, totalTokens, substituted := usageFromResponsesJSON(response)
	r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: 200,
		DurationMs: time.Since(started).Milliseconds(), InputTokens: inputTokens,
		OutputTokens: outputTokens, TotalTokens: totalTokens,
		EstimatedInputTokens: int64(substituted)})
}

func (r *Runner) runStream(request Request, target string, headers map[string]string,
	body []byte, translator wire.ResponseTranslator, prepared *wire.Request, nsIndex *translate.NamespaceIndex,
	estimate int, providerID string, started time.Time) {
	relay := NewStreamRelay(request.Sink)
	streamOpts := wire.StreamOptions{SessionModel: request.Model.Slug,
		EstimateInput: estimate, NamespaceIndex: nsIndex, CustomTools: prepared.CustomTools}
	first, firstErr := r.RunAttempt(request.Context, target, headers, body, request.Model, translator, streamOpts, relay)
	if firstErr != nil {
		var failure *UpstreamFailure
		if errors.As(firstErr, &failure) {
			r.writeUpstreamError(request, failure, started)
			r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
				Status: failure.Status, DurationMs: time.Since(started).Milliseconds()})
			return
		}
		r.failLiveStream(request, firstErr, relay, first.Translator, started, usage.Event{})
		return
	}

	if !first.Translator.HasContent() {
		if !relay.HeadersWritten() && !relay.HasWriteError() && request.Context.Err() == nil {
			if err := relay.FinishFlushWith(stripTerminalCompletion(first.Events.Bytes())); err != nil {
				r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
					Status: 0, DurationMs: time.Since(started).Milliseconds()})
				return
			}
		}
		if relay.HeadersWritten() {
			_ = request.Sink.Write([]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"empty_completion\",\"message\":\"The model returned an empty completion. The router did not retry it.\"}}}\n\n"))
			_ = request.Sink.Write([]byte("data: [DONE]\n\n"))
			request.Sink.Flush()
		}
		r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
			Status: http.StatusBadGateway, DurationMs: time.Since(started).Milliseconds(), EmptyCompletion: true})
		return
	}

	if !relay.HeadersWritten() && !relay.HasWriteError() && request.Context.Err() == nil {
		if err := relay.FinishFlushWith(first.Events.Bytes()); err != nil {
			r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID,
				Status: 0, DurationMs: time.Since(started).Milliseconds()})
			return
		}
	}
	r.record(request, usage.Event{Model: request.Model.Slug, Provider: providerID, Status: 200,
		DurationMs: time.Since(started).Milliseconds(), InputTokens: first.Translator.PromptTokens(),
		OutputTokens: first.Translator.OutputTokens(), TotalTokens: first.Translator.TotalTokens(),
		EstimatedInputTokens: int64(first.Translator.SubstitutedInputTokens())})
	r.logInfo(request, "request done", "model", request.Model.Slug, "provider", request.Provider.ID,
		"status", 200, "duration_ms", time.Since(started).Milliseconds(),
		"in", first.Translator.PromptTokens(), "out", first.Translator.OutputTokens(),
		"estimated_input", first.Translator.SubstitutedInputTokens() > 0)
}

func (r *Runner) failLiveStream(request Request, err error, relay *StreamRelay,
	translator wire.StreamTranslator, started time.Time, base usage.Event) {
	idle := errors.Is(err, httpx.ErrUpstreamIdle)
	status := http.StatusBadGateway
	if idle {
		status = http.StatusGatewayTimeout
	}
	base.Model, base.Provider = request.Model.Slug, request.Provider.ID
	base.Status = status
	base.DurationMs = time.Since(started).Milliseconds()
	base.UpstreamIdle = idle
	if relay != nil && (relay.HeadersWritten() || relay.HasWriteError()) {
		base.StreamAborted = true
		if translator != nil && translator.HasToolCalls() {
			_ = request.Sink.Write([]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"stream_interrupted_after_tool_call\",\"message\":\"The upstream stream died after tool calls had already been delivered. The router closed it with an explicit failure instead of letting the client retry the whole turn, because a retry would re-execute those tool calls.\"}}}\n\n"))
			request.Sink.Flush()
		}
		r.logError(request, "stream truncated", "model", request.Model.Slug, "provider", request.Provider.ID,
			"status", status, "duration_ms", base.DurationMs, "upstream_idle", idle,
			"tool_calls", translator != nil && translator.HasToolCalls(), "error", err)
		r.record(request, base)
		return
	}
	if idle {
		request.Sink.WriteJSON(status, errBody("upstream_idle_timeout",
			"The upstream stream produced no data within the configured idle window; the router closed it so the client can retry."))
	} else {
		request.Sink.WriteJSON(status, errBody("provider_api_proxy_error",
			"The API-provider forwarder could not complete the request."))
	}
	r.logError(request, "request failed", "model", request.Model.Slug, "provider", request.Provider.ID,
		"status", status, "duration_ms", base.DurationMs, "upstream_idle", idle, "error", err)
	r.record(request, base)
}

func (r *Runner) writeUpstreamError(request Request, failure *UpstreamFailure, started time.Time) {
	truncated := failure.BodyText
	if len(truncated) > 200 {
		truncated = truncated[:200]
	}
	r.logWarn(request, "upstream error", "model", request.Model.Slug, "provider", request.Provider.ID,
		"status", failure.Status, "duration_ms", time.Since(started).Milliseconds(),
		"upstream_error", truncated)
	if failure.RetryAfter > 0 {
		request.Sink.SetHeader("Retry-After", fmt.Sprintf("%d", failure.RetryAfter))
	}
	payload := errBody("provider_api_proxy_error", "The API-provider forwarder returned an error.")
	if r.TranslateProviderError != nil {
		payload = r.TranslateProviderError(failure.Status, failure.BodyText,
			request.Model.DisplayName, request.Provider.DisplayName, request.Provider.ID, failure.RetryAfter)
	}
	request.Sink.WriteJSON(failure.Status, payload)
}

func (r *Runner) relayResponse(request Request, sink Sink, resp *http.Response) {
	skip := map[string]bool{"content-length": true, "transfer-encoding": true,
		"connection": true, "keep-alive": true}
	header := make(http.Header)
	for name, values := range resp.Header {
		if skip[strings.ToLower(name)] {
			continue
		}
		for _, value := range values {
			header.Add(name, value)
		}
	}
	sink.WriteHeaders(resp.StatusCode, header)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if writeErr := sink.Write(buf[:n]); writeErr != nil {
				return
			}
			sink.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.logWarn(request, "relay truncated", "status", resp.StatusCode,
					"upstream_idle", errors.Is(err, httpx.ErrUpstreamIdle), "error", err)
			}
			return
		}
	}
}

func readLimited(body io.Reader, limit int64) ([]byte, error, bool) {
	if limit <= 0 {
		limit = 1 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	return raw, err, int64(len(raw)) > limit
}

func readBody(body io.Reader, limit int64) string {
	raw, _, _ := readLimited(body, limit)
	return string(raw)
}

func retryAfter(header http.Header) int {
	value := 0
	if raw := header.Get("Retry-After"); raw != "" {
		_, _ = fmt.Sscanf(raw, "%d", &value)
	}
	return value
}

func stripTerminalCompletion(buf []byte) []byte {
	blocks := strings.Split(string(buf), "\n\n")
	kept := blocks[:0]
	for _, block := range blocks {
		if block == "data: [DONE]" || strings.HasPrefix(block, "event: response.completed") {
			continue
		}
		kept = append(kept, block)
	}
	return []byte(strings.Join(kept, "\n\n"))
}

func usageFromResponsesJSON(response map[string]any) (int64, int64, int64, int) {
	usageField, _ := response["usage"].(map[string]any)
	read := func(key string) int64 {
		if value, ok := usageField[key].(float64); ok {
			return int64(value)
		}
		return 0
	}
	return read("input_tokens"), read("output_tokens"), read("total_tokens"), 0
}
