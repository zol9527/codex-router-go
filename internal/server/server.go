// Package server 是 Go 版 router：单进程监听 4202，把 Codex 的
// Responses 流量分流到 native ChatGPT 后端或翻译后发往 chat 上游。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
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
	ListenAddr  string // "127.0.0.1:4202"
	NativeBase  string // 默认 https://chatgpt.com/backend-api/codex
}

// Server 持有全部共享状态。
type Server struct {
	opt       Options
	client    *http.Client
	callerKey string

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
}

// New 构造 Server。
func New(opt Options) (*Server, error) {
	if opt.NativeBase == "" {
		opt.NativeBase = "https://chatgpt.com/backend-api/codex"
	}
	callerKey, err := opt.State.CallerKey()
	if err != nil {
		return nil, err
	}
	return &Server{
		opt:       opt,
		callerKey: callerKey,
		client: &http.Client{
			// 上游思考型模型可能长时间不吐首字节；取消由请求上下文管理，
			// 这里不设全局超时（与 Node 版 fetch 行为一致）。
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
		active: map[int]*activityEntry{},
	}, nil
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

// beginRequest 登记 activity 并返回结束函数。
func (s *Server) beginRequest() (setRoute func(provider, model, session string), finish func(status int)) {
	s.mu.Lock()
	s.requestSeq++
	id := s.requestSeq
	started := time.Now()
	entry := &activityEntry{id: strconv.Itoa(id), startedAt: started}
	s.active[id] = entry
	s.mu.Unlock()

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
		if now.Sub(entry.startedAt) > staleActivity {
			delete(s.active, id)
		}
	}
	active := make([]any, 0, len(s.active))
	for _, entry := range s.active {
		if entry.provider == "" {
			continue
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
	for _, model := range s.opt.Registry.Models {
		if !model.Listed {
			continue
		}
		if !s.opt.State.ProviderEnabled(model.Provider, s.opt.Registry.CanonicalProviderID) {
			continue
		}
		ownedBy := "openai"
		if p := s.opt.Registry.ProviderFor(model); p != nil {
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

var _ = path.Clean // 保留引用（路由匹配用字符串精确比较）
var _ = fmt.Sprintf
