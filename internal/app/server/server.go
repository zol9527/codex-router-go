// Package server 是 Go 版 router：单进程监听 4202，把 Codex 的
// Responses 流量分流到 native ChatGPT 后端或翻译后发往 chat 上游。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loyd/codex-router/internal/domain/cred"
	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/state"
	"github.com/loyd/codex-router/internal/domain/usage"
	"github.com/loyd/codex-router/internal/domain/vision"
	"github.com/loyd/codex-router/internal/domain/wire"
	"github.com/loyd/codex-router/internal/engine/nativebackend"
	"github.com/loyd/codex-router/internal/engine/routing"
)

// CallerPathPrefix 与 Node 版一致：caller key 以 URL 路径形式出现。
const CallerPathPrefix = "/_codex-router"

// Version 由 main 注入（构建信息）。
var Version = "dev"

// Options 装配 server 所需的全部依赖。
type Options struct {
	State       *state.State
	Registry    *registry.Registry
	Credentials *cred.Resolver
	ListenAddr  string                // "127.0.0.1:4202"
	NativeBase  string                // 默认 https://chatgpt.com/backend-api/codex
	Usage       *usage.Recorder       // usage-events.jsonl 管道（可空）
	RateLimits  *usage.RateLimitStore // rate-limits.json 收割（可空）
	// DisableWebSocketPassthrough 关闭 WS 透传（回滚开关）：/responses
	// 上的升级握手一律 426，调用方全部走 HTTP。默认开启透传。
	DisableWebSocketPassthrough bool
	// UpstreamHeaderTimeout 是上游响应头超时（请求写完到首字节响应头）。
	// 0 = 默认档；负值 = 关闭。背景：2026-08-16 zai 黑洞 —— 上游 505s
	// 才回 500，无此超时只能靠用户手动停（GLM turn 挂死 8 分 26 秒事故）。
	UpstreamHeaderTimeout time.Duration
	// UpstreamIdleTimeout 是上游响应体看门狗窗口（SSE 流上连续无字节
	// 即断开，错误链带 httpx.ErrUpstreamIdle）。0 = 默认档；负值 = 关闭。
	UpstreamIdleTimeout time.Duration
	// WSSilentTimeout 是 WS 透传管道的方向相关看门狗窗口：客户端发过
	// 请求帧而上游此后零回帧超过该窗口 → 主动拆管（调用方重连自愈）。
	// 跨 turn 空闲不拆。0 = 默认档；负值 = 关闭。
	WSSilentTimeout time.Duration
	// WSKeepaliveInterval 是上游 WS 管道的 keepalive ping 周期：空闲
	// 管道周期性向上游发 ping，避免中间设备（NAT/TUN）按空闲超时砍断
	// （客户端腿是本地回环，不发）。0 = 默认档；负值 = 关闭。
	WSKeepaliveInterval time.Duration
	// SlowRequestLogDelay 是慢请求可见性看门狗：请求在途超过该窗口
	// 仍无收尾日志时补一行 `slow request pending`（只记录、不拆流）。
	// 背景：2026-08-16 Surge fake-IP 把 TLS 握手黑洞，请求永久挂死且
	// 零日志（完成/失败日志都只在收尾打），只能靠 /health 数僵尸定位。
	// 0 = 默认档；负值 = 关闭。
	SlowRequestLogDelay time.Duration
}

// Server 持有全部共享状态。
type Server struct {
	opt       Options
	client    *http.Client
	callerKey string
	// upstreamIdle 是解析后的响应体看门狗窗口（0=关闭）。它只将无
	// 字节流转换为明确错误，不在 Router 内重放请求。
	upstreamIdle time.Duration
	// wsSilent 是解析后的 WS 管道看门狗窗口（0=关闭）。
	wsSilent time.Duration
	// wsKeepalive 是解析后的上游 keepalive ping 周期（0=关闭）。
	wsKeepalive time.Duration
	// wsKeepaliveIdleCap 是空闲管道的保活封顶：连续无数据帧超过该窗口
	// 的管道主动干净关闭（防调用方泄漏管道时无限累积）。
	wsKeepaliveIdleCap time.Duration
	// slowRequestLog 是解析后的慢请求日志窗口（0=关闭）。
	slowRequestLog time.Duration
	// reg 是当前生效的注册表；SIGUSR1 热重载（动态注册模型后）整体
	// 换指针 —— 请求路径只读，RWMutex 足够。
	regMu sync.RWMutex
	reg   *registry.Registry
	// native 是 Native Backend adapter；server 只表达 HTTP transport 语义。
	native nativebackend.Client
	// routeRunner 承载一次完整 Routing Turn；HTTP handler 只做 transport。
	routeRunner *routing.Runner

	// visionCache 是会话级读图缓存：Codex 每轮重发完整历史，同一
	// (session, ImageKey) 只在首次调读图引擎（详见 vision 包注释）。
	visionCache *vision.SessionCache

	mu           sync.Mutex
	active       map[int]*activityEntry
	requestSeq   int
	lastProvider string
	lastModel    string
	lastSession  string
	errorUntil   time.Time
}

type activityEntry struct {
	id          string
	provider    string
	model       string
	sessionName string
	startedAt   time.Time
	// inFlight 非 nil 时（WS 管道这类跨 turn 的长生命周期载体），以
	// 回调结果决定是否计入 active：管道空闲（无在途 turn）不算请求。
	// nil（HTTP 单请求路径）恒计入。2026-08-18 实发：Codex 每次对话
	// 开一条 WS preconnect 且长期不关，管道级 activity 让 state 永远
	// generating，托盘动画永不回 idle。
	inFlight func() bool
}

// New 构造 Server。
// registry 返回当前生效的注册表（请求路径只读；SIGUSR1 热重载换指针）。
func (s *Server) registry() *registry.Registry {
	s.regMu.RLock()
	defer s.regMu.RUnlock()
	return s.reg
}

// SetRegistry 原子替换注册表（SIGUSR1 热重载入口）。
func (s *Server) SetRegistry(next *registry.Registry) {
	s.regMu.Lock()
	s.reg = next
	s.regMu.Unlock()
}

// setNativeBase 同步测试注入的 Native Backend 地址；生产路径 base 在
// New 时固定，不通过该入口变更。
func (s *Server) setNativeBase(base string) {
	s.opt.NativeBase = base
	if backend, ok := s.native.(*nativebackend.Backend); ok {
		s.native = nativebackend.New(base, s.client, s.upstreamIdle, backend.IsLocalToken)
	}
}

// 上游 fail-fast 默认档（对齐 AI SDK 语义：把无限挂起变成可重试的
// 快速失败）。档位取保守值：GLM max effort + 90k 上下文的最长健康
// 请求约 90s，300s 响应头窗口极难误伤；SSE 思考增量连续吐，180s
// 零字节只可能是挂死。均可经 main 的 env 覆盖。
const (
	DefaultUpstreamHeaderTimeout = 300 * time.Second
	DefaultUpstreamIdleTimeout   = 180 * time.Second
	// DefaultWSSilentTimeout：上游 response.created 正常 ~1s 内到达，
	// 60s 极保守 —— 触发即认定上游侧黑洞（见 wsWatchdog）。
	DefaultWSSilentTimeout = 60 * time.Second
	// DefaultWSKeepaliveInterval：上游 WS 管道的 keepalive ping 周期。
	// 2026-08-19 实证：上游腿经 Surge TUN，空闲管道在 ~30.05 分钟被
	// 中间设备按空闲超时砍断（194 条管道 1006 unexpected EOF / RST），
	// 下一次使用才发现管道已死。60s ping 让链路始终有流量，远低于
	// 任何常见 NAT/TUN 空闲窗口。
	DefaultWSKeepaliveInterval = 60 * time.Second
	// DefaultWSKeepaliveIdleCap：keepalive 不设无限保活 —— 空闲超过该
	// 窗口的管道主动干净关闭。正常会话的跨 turn 间隙远短于 2 小时；
	// 泄漏管道（调用方 bug）2 小时后回收，防无限累积。
	DefaultWSKeepaliveIdleCap = 2 * time.Hour
	// DefaultSlowRequestLogDelay：健康长请求（GLM max effort + 大上下文）
	// 约 90s 完成，120s 只记真正的悬挂、不误伤慢而正常的流。
	DefaultSlowRequestLogDelay = 120 * time.Second
)

// resolveTimeout 把 Options 的三态（0=默认 / 负=关闭 / 正=指定）解析成
// 实际生效值。关闭时返回 0（http.Transport 的 ResponseHeaderTimeout
// 与看门狗窗口都以零值表示不超时）。
func resolveTimeout(v, def time.Duration) time.Duration {
	switch {
	case v > 0:
		return v
	case v < 0:
		return 0
	default:
		return def
	}
}

func New(opt Options) (*Server, error) {
	if opt.NativeBase == "" {
		opt.NativeBase = "https://chatgpt.com/backend-api/codex"
	}
	callerKey, err := opt.State.CallerKey()
	if err != nil {
		return nil, err
	}
	headerTimeout := resolveTimeout(opt.UpstreamHeaderTimeout, DefaultUpstreamHeaderTimeout)
	idleTimeout := resolveTimeout(opt.UpstreamIdleTimeout, DefaultUpstreamIdleTimeout)
	wsSilent := resolveTimeout(opt.WSSilentTimeout, DefaultWSSilentTimeout)
	wsKeepalive := resolveTimeout(opt.WSKeepaliveInterval, DefaultWSKeepaliveInterval)
	slowLog := resolveTimeout(opt.SlowRequestLogDelay, DefaultSlowRequestLogDelay)
	client := &http.Client{
		// 上游思考型模型可能长时间不吐首字节 —— 但"永远不吐"必须
		// fail-fast：响应头窗口由 ResponseHeaderTimeout 把关（计时
		// 从请求写完到首字节响应头，不含 body 流式时长），body 挂死
		// 由 httpx.Fetch 的空闲看门狗把关。取消仍由请求上下文管理，
		// 这里依旧不设全局超时。
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			// TLS 握手窗口：经 Surge/Clash 类 TUN 代理时 TCP 在
			// fake-IP 层秒连（拨号超时不触发），代理节点抖动会把
			// TLS 握手黑洞成"连接 ESTABLISHED 但永不完成"——此处
			// 不设超时则请求永久挂死且零日志（2026-08-16 commit
			// 生成全挂事故：6 个僵尸请求只靠 /health 才数得出来）。
			// 档位与拨号 30s 同类，固定值、不开 env。
			TLSHandshakeTimeout:   15 * time.Second,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: headerTimeout,
		},
	}
	internalKey := opt.State.InternalKey()
	native := nativebackend.New(opt.NativeBase, client, idleTimeout, func(header string) bool {
		token := nativebackend.BearerToken(header)
		if token == "" {
			return false
		}
		return subtleEqual(token, callerKey) || (internalKey != "" && subtleEqual(token, internalKey))
	})
	server := &Server{
		opt:                opt,
		callerKey:          callerKey,
		reg:                opt.Registry,
		native:             native,
		upstreamIdle:       idleTimeout,
		wsSilent:           wsSilent,
		wsKeepalive:        wsKeepalive,
		wsKeepaliveIdleCap: DefaultWSKeepaliveIdleCap,
		slowRequestLog:     slowLog,
		visionCache:        vision.NewSessionCache(vision.SessionCacheCapacity),
		client:             client,
		active:             map[int]*activityEntry{},
	}
	server.routeRunner = &routing.Runner{
		Registry:       server.registry,
		Client:         server.client,
		Idle:           server.upstreamIdle,
		Version:        Version,
		Recorder:       server.opt.Usage,
		RateLimits:     server.opt.RateLimits,
		NormalizeInput: server.normalizeRoutedAgentInput,
		BridgeVision: func(ctx context.Context, header http.Header, payload map[string]any, model *registry.Model) {
			server.bridgeVision(ctx, header, payload, model)
		},
		ProviderBaseURL:        server.providerBaseURL,
		TranslateProviderError: translateProviderError,
		LogTranslationDegraded: func(prepared *wire.Request, model *registry.Model) {
			logTranslationDegradation(prepared, model)
		},
		Logf: logf,
	}
	return server, nil
}

// Handler 返回根路由处理器。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handle)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// /health 无认证 —— tray 每 350ms 轮询它。
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		s.handleHealth(w)
		return
	}

	route, ok := s.authenticate(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{
				"type":    "authentication_error",
				"message": "This local router endpoint requires its configured caller capability.",
			},
		})
		return
	}

	switch {
	case r.Method == http.MethodGet && (route == "/health" || route == "/v1/health"):
		s.handleHealth(w)
	case r.Method == http.MethodGet && (route == "/models" || route == "/v1/models"):
		s.handleModels(w)
	case r.Method == http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && isResponsesRoute(route) && isWebSocketUpgrade(r):
		// WS 升级请求进专用处理器分流：路由模型 → 426 干净回退；
		// 原生模型 → 上游 WSS 双向管道。状态码契约见 wsproxy.go。
		s.handleResponsesWebSocket(w, r, route)
	case r.Method == http.MethodPost && isResponsesRoute(route):
		s.handleResponses(w, r, route)
	case r.Method == http.MethodPost && isNativeImagePath(route):
		s.handleNative(w, r, route, "gpt-image-2")
	case r.Method == http.MethodPost && isNativeSearchPath(route):
		s.handleNative(w, r, route, "web-search")
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"type": "proxy_route_not_found", "message": "Unsupported router route.",
			},
		})
	}
}

func isResponsesRoute(route string) bool {
	return route == "/responses" || route == "/v1/responses" ||
		route == "/responses/compact" || route == "/v1/responses/compact"
}

// isWebSocketUpgrade 判断是否为 RFC 6455 升级握手
// （GET + Connection: Upgrade + Upgrade: websocket）。
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

var nativeImagePaths = map[string]bool{
	"/images/edits": true, "/images/generations": true,
	"/v1/images/edits": true, "/v1/images/generations": true,
}

var nativeSearchPaths = map[string]bool{
	"/alpha/search": true, "/v1/alpha/search": true,
}

func isNativeImagePath(route string) bool  { return nativeImagePaths[route] }
func isNativeSearchPath(route string) bool { return nativeSearchPaths[route] }

// authenticate 校验 /_codex-router/<secret>/v1/... 前缀。
// 匹配时返回去掉前缀后的路由（如 /responses）。
func (s *Server) authenticate(pathname string) (string, bool) {
	prefix := CallerPathPrefix + "/"
	if !strings.HasPrefix(pathname, prefix) {
		return "", false
	}
	remainder := pathname[len(prefix):]
	separator := strings.Index(remainder, "/")
	if separator == -1 {
		return "", false
	}
	candidate := remainder[:separator]
	// 常数时间比较：URL 路径是调用方可控输入。
	if len(candidate) != len(s.callerKey) ||
		subtle.ConstantTimeCompare([]byte(candidate), []byte(s.callerKey)) != 1 {
		return "", false
	}
	route := remainder[separator:]
	if route == "" {
		route = "/"
	}
	return route, true
}

// requireCodexTransport 拒绝浏览器形态请求并要求 JSON Content-Type。
func requireCodexTransport(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": map[string]any{
				"type":    "browser_request_rejected",
				"message": "Browser-originated requests are not accepted by the local model router.",
			},
		})
		return false
	}
	contentType := r.Header.Get("Content-Type")
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	if strings.ToLower(strings.TrimSpace(contentType)) != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{
			"error": map[string]any{
				"type":    "unsupported_media_type",
				"message": "Codex router requests require Content-Type: application/json.",
			},
		})
		return false
	}
	return true
}

// ---- activity（tray 的 /health 轮询负载）----

const staleActivity = 15 * time.Minute
const errorStatusDuration = 8 * time.Second

// beginRequest 登记 activity 并返回结束函数（HTTP 单请求路径：
// 登记即视为在途，直到 finish）。
func (s *Server) beginRequest() (setRoute func(provider, model, session string), finish func(status int)) {
	return s.beginActivity(nil)
}

// beginActivity 登记一条 activity；inFlight 语义见 activityEntry.inFlight。
// WS 管道路径传入看门狗的在途 turn 判定，把上报窗口从"管道存活"
// 收窄成"turn 在途"。
func (s *Server) beginActivity(inFlight func() bool) (setRoute func(provider, model, session string), finish func(status int)) {
	s.mu.Lock()
	s.requestSeq++
	id := s.requestSeq
	started := time.Now()
	entry := &activityEntry{id: strconv.Itoa(id), startedAt: started, inFlight: inFlight}
	s.active[id] = entry
	s.mu.Unlock()

	// 慢请求看门狗：完成/失败日志都只在收尾打，挂死请求在 router.log
	// 里零痕迹（见 SlowRequestLogDelay 的背景注释）。在途超过窗口补
	// 一行可见性 —— 只记录、不拆流，真正的 fail-fast 由超时闸负责。
	var watchdog *time.Timer
	if s.slowRequestLog > 0 {
		watchdog = time.AfterFunc(s.slowRequestLog, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, pending := s.active[id]; !pending {
				return // 与 finish 赛跑落败：请求已收尾，不误报
			}
			if entry.inFlight != nil && !entry.inFlight() {
				return // 长连接载体当前无在途 turn：空闲管道不是慢请求
			}
			logf("slow request pending id=%s provider=%s model=%s session=%s elapsed_ms=%d",
				entry.id, entry.provider, entry.model, entry.sessionName,
				time.Since(entry.startedAt).Milliseconds())
		})
	}

	var once sync.Once
	setRoute = func(provider, model, session string) {
		if provider == "" {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		entry.provider = provider
		if model != "" {
			entry.model = model
			s.lastModel = model
		}
		if session != "" {
			entry.sessionName = session
			s.lastSession = session
		}
		s.lastProvider = provider
	}
	finish = func(status int) {
		once.Do(func() {
			if watchdog != nil {
				watchdog.Stop()
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.active, id)
			if status >= 400 {
				s.errorUntil = time.Now().Add(errorStatusDuration)
			}
		})
	}
	return setRoute, finish
}

func (s *Server) activityPayload() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, entry := range s.active {
		// 带 inFlight 的条目（WS 管道）生命周期归管道管理（拆管时
		// finish）；按 startedAt 过期会把仍健康的管道踢出 active，
		// 之后管道上的新 turn 永远不上报。挂死的管道由 WS 静默看门狗
		// 拆管收尾，不依赖这里的 stale 兜底。
		if entry.inFlight == nil && now.Sub(entry.startedAt) > staleActivity {
			delete(s.active, id)
		}
	}
	active := make([]any, 0, len(s.active))
	for _, entry := range s.active {
		if entry.provider == "" {
			continue
		}
		if entry.inFlight != nil && !entry.inFlight() {
			continue // 空闲长连接载体：无在途 turn，不报 generating
		}
		item := map[string]any{
			"id": entry.id, "provider": entry.provider, "startedAt": entry.startedAt.UnixMilli(),
		}
		if entry.model != "" {
			item["model"] = entry.model
		}
		if entry.sessionName != "" {
			item["sessionName"] = entry.sessionName
		}
		active = append(active, item)
	}
	state := "idle"
	if len(active) > 0 {
		state = "generating"
	} else if now.Before(s.errorUntil) {
		state = "error"
	}
	payload := map[string]any{
		"state":       state,
		"activeCount": len(active),
		"active":      active,
	}
	provider, model, session := s.lastProvider, s.lastModel, s.lastSession
	if provider != "" {
		payload["provider"] = provider
	}
	if model != "" {
		payload["model"] = model
	}
	if session != "" {
		payload["sessionName"] = session
	}
	return payload
}

func (s *Server) handleHealth(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"service":  "codex-router",
		"version":  Version,
		"router":   "ready",
		"activity": s.activityPayload(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter) {
	data := []any{}
	for _, model := range s.registry().Models {
		if !model.Listed {
			continue
		}
		if !s.opt.State.ProviderEnabled(model.Provider, s.registry().CanonicalProviderID) {
			continue
		}
		ownedBy := "openai"
		if p := s.registry().ProviderFor(model); p != nil {
			ownedBy = p.OwnedBy
		}
		data = append(data, map[string]any{
			"id": model.Slug, "object": "model", "owned_by": ownedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"error":{"type":"internal","message":"encode failure"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)
	w.Write(raw)
}

// sessionNameFromHeaders 提取 Codex 会话名（activity 元数据）。
func sessionNameFromHeaders(header http.Header) string {
	raw := header.Get("X-Codex-Turn-Metadata")
	if raw == "" {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return ""
	}
	if name, ok := meta["session_name"].(string); ok && strings.TrimSpace(name) != "" {
		return name
	}
	return ""
}

// logf 统一服务日志（时间戳 + 组件前缀，等价 Node 版 console.error）。
func logf(format string, args ...any) {
	log.Printf("[codex-router] "+format, args...)
}
