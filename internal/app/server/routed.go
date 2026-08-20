package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"time"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/usage"
	"github.com/loyd/codex-router/internal/domain/wire"
	"github.com/loyd/codex-router/internal/engine/nativebackend"
	"github.com/loyd/codex-router/internal/engine/routing"
	"github.com/loyd/codex-router/internal/lib/httpx"
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
	normalizeLegacyCustomToolIDs(payload)
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
		s.handleRoutedCompaction(w, r, payload, routeModel, provider, credential, compactV2, started)
		return
	}

	// 全部 routed 流量进入 Routing Turn；HTTP server 只提供认证后的
	// request metadata 与 response sink。
	runner := *s.routeRunner
	// usage/rate-limit 依赖可在测试与热配置中替换；按请求复制 runner，
	// 避免并发请求共享可变 recorder 指针。
	runner.Recorder = s.opt.Usage
	runner.RateLimits = s.opt.RateLimits
	runner.Client = s.client
	runner.Idle = s.upstreamIdle
	runner.Run(routing.Request{
		Context: r.Context(), Payload: payload, Model: routeModel,
		Provider: provider, Credential: credential, Sink: responseSink{w: w},
		Header: r.Header, Started: started,
	})
}

// handleNativeTurn：未命中注册表的模型按 native 流量直连。
func (s *Server) handleNativeTurn(w http.ResponseWriter, r *http.Request, route string,
	payload map[string]any, requestedModel string, setRoute func(string, string, string), started time.Time) {

	setRoute("openai", requestedModel, sessionNameFromHeaders(r.Header))
	native := payload
	if !s.native.CallerHasCredential(r.Header) {
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

// responseSink 把 routing 的流式写回接缝绑定到 net/http。
type responseSink struct{ w http.ResponseWriter }

func (s responseSink) SetHeader(name, value string) { s.w.Header().Set(name, value) }
func (s responseSink) WriteHeaders(status int, header http.Header) {
	for name, values := range header {
		for _, value := range values {
			s.w.Header().Add(name, value)
		}
	}
	s.w.WriteHeader(status)
}
func (s responseSink) WriteJSON(status int, payload any) { writeJSON(s.w, status, payload) }
func (s responseSink) WriteSSEHeader() {
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.WriteHeader(http.StatusOK)
}
func (s responseSink) Write(chunk []byte) error {
	_, err := s.w.Write(chunk)
	return err
}
func (s responseSink) Flush() {
	if flusher, ok := s.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// logTranslationDegradation 报告翻译期降级：input item / content part
// 出现了翻译器不认识的类型，被占位符替换或丢弃。这类静默内容损坏
// 不影响 HTTP 状态（全程 200、usage 正常），没有日志就无迹可循——
// 2026-08-18 agent_message 任务书被吞的事故因此排查了两轮。
//
// 同一签名（模型 + 类型集合）2 秒窗口内只记一次，被抑制的次数
// 累计进下一条（suppressed=N），信息不丢、风暴不刷屏。写盘本身
// 无性能压力（标准 log 每行一次缓冲写、无 fsync，实测日均约
// 4 行/分钟、峰值 12 行/分钟），限流纯粹为了日志可读性。
var (
	degradationLogMu         sync.Mutex
	degradationLogInterval   = 2 * time.Second
	degradationLogLast       = map[string]time.Time{}
	degradationLogSuppressed = map[string]int{}
)

func logTranslationDegradation(prepared *wire.Request, model *registry.Model) {
	if len(prepared.OmittedItemTypes) == 0 && len(prepared.OmittedPartTypes) == 0 {
		return
	}
	slug := ""
	if model != nil {
		slug = model.Slug
	}
	signature := fmt.Sprintf("%s|%v|%v", slug, prepared.OmittedItemTypes, prepared.OmittedPartTypes)
	now := time.Now()

	degradationLogMu.Lock()
	defer degradationLogMu.Unlock()
	if last, seen := degradationLogLast[signature]; seen && now.Sub(last) < degradationLogInterval {
		degradationLogSuppressed[signature]++
		return
	}
	suppressed := degradationLogSuppressed[signature]
	delete(degradationLogSuppressed, signature)
	degradationLogLast[signature] = now
	if suppressed > 0 {
		logf("chat translation degraded model=%s omitted_item_types=%v omitted_part_types=%v suppressed=%d/2s",
			slug, prepared.OmittedItemTypes, prepared.OmittedPartTypes, suppressed)
		return
	}
	logf("chat translation degraded model=%s omitted_item_types=%v omitted_part_types=%v",
		slug, prepared.OmittedItemTypes, prepared.OmittedPartTypes)
}

// recordTurn 是 usage 记录的统一入口（nil-safe）。
func (s *Server) recordTurn(event usage.Event) {
	if s.opt.Usage != nil {
		s.opt.Usage.Record(event)
	}
}

// providerBaseURL 解析 provider 的上游地址（env 覆盖 > config base_url >
// 注册表默认）。config 档服务自托管 provider（如 LiteLLM）：注册表不
// 内嵌部署地址，运行时从 config.toml 的 provider 表读取。
func (s *Server) providerBaseURL(p *registry.Provider) string {
	if p.BaseURLEnv != "" {
		if v := os.Getenv(p.BaseURLEnv); v != "" {
			return v
		}
	}
	family := p.ID
	if p.VariantOf != "" {
		family = p.VariantOf
	}
	if s.opt.State != nil {
		if v := strings.TrimSpace(s.opt.State.ReadConfigBaseURL(family)); v != "" {
			return v
		}
	}
	return p.BaseURL
}

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}
