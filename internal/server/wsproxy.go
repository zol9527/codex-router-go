// wsproxy.go —— Responses-over-WebSocket 透传（原生模型专用）。
//
// Codex 的状态码契约（2026-08-15 从 openai/codex core/src/client.rs
// 确认）：握手回 426 时调用方不进重试梯子，当轮直接切 HTTP 并把整个
// 会话粘在 HTTP 上（FallbackToHttp 出口）；回 404 则被当普通流错误
// 烧满 5 次重试（实测 ~7 秒 + "正在重新连接 N/5" 横幅）。
//
// 分流规则：
//   - 路由模型（x-codex-routing-hint 的 model 带 provider 前缀）→ 426。
//     路由上游是无状态 chat completions，WS 的 previous_response_id
//     增量上传收益为零，不值得为此模拟响应链状态。
//   - 原生模型 / 无 hint（preconnect）→ 先拨上游 WSS，拨通才对调用方
//     回 101；拨不通同样回 426（调用方干净回退 HTTP，而非握死管道）。
//   - 已建立的原生管道上出现路由帧（线程中途换模型的边缘情况）→
//     主动断线；调用方带 hint 重连会再次命中 426 分支，会话切 HTTP 自愈。
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// wsMaxFrameBytes：单帧上限。Responses 请求含完整 instructions/tools，
// 几十 KB 是常态；64MB 是宽松护栏而非目标尺寸。
const wsMaxFrameBytes = 64 << 20

// wsDialTimeout：上游拨号超时，比 Codex 默认连接超时（15s）留余量。
const wsDialTimeout = 12 * time.Second

var wsUpgrader = websocket.Upgrader{
	// 调用方已经过 caller key 路径认证，本地回环没有浏览器 Origin 攻击面。
	CheckOrigin: func(r *http.Request) bool { return true },
}

var wsDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: wsDialTimeout,
	// 不协商 permessage-deflate：管道两端各自压缩会破坏字节级透传。
	EnableCompression: false,
}

// errRoutedFrameOnNativePipe：原生管道上出现路由帧的哨兵错误。
var errRoutedFrameOnNativePipe = errors.New("routed frame on native pipe")

// writeWebSocketUnsupported 回 426 —— Codex 的干净回退信号（契约见文件头）。
func writeWebSocketUnsupported(w http.ResponseWriter) {
	w.Header().Set("Upgrade", "websocket")
	w.Header().Set("Connection", "Upgrade")
	writeJSON(w, http.StatusUpgradeRequired, map[string]any{
		"error": map[string]any{
			"type":    "websocket_transport_unsupported",
			"message": "This connection is served over HTTP only.",
		},
	})
}

// handleResponsesWebSocket 处理 /responses 上的 WebSocket 升级请求。
func (s *Server) handleResponsesWebSocket(w http.ResponseWriter, r *http.Request, route string) {
	if s.opt.DisableWebSocketPassthrough {
		// 回滚开关（CODEX_WS_PASSTHROUGH=0）：整体退回"一律 426"。
		writeWebSocketUnsupported(w)
		return
	}
	// 路由模型：WS 无收益，426 让调用方干净回退 HTTP。
	if model := parseRoutingHintModel(r.Header.Get("X-Codex-Routing-Hint")); isRoutedSlug(model) {
		writeWebSocketUnsupported(w)
		return
	}
	target := wsUpstreamURL(s.nativeTarget(route))
	header := http.Header{}
	for name, value := range s.callerForwardHeaders(r) {
		header.Set(name, value)
	}
	if hint := r.Header.Get("X-Codex-Routing-Hint"); hint != "" {
		// 握手级路由提示是 WS 协议形状的一部分（连接建立前告知后端
		// 目标模型）；HTTP 透传不转发它，WS 握手按 Codex 原生行为转发。
		header.Set("X-Codex-Routing-Hint", hint)
	}
	upstream, resp, err := wsDialer.Dial(target, header)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		logf("ws upstream dial failed host=%s err=%v", hostOf(target), err)
		// 上游不可达/拒绝升级 → 426 回退，而不是建立一条死管道。
		writeWebSocketUnsupported(w)
		return
	}
	client, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		upstream.Close()
		// Upgrade 已向调用方写出错误响应，无需再回 426。
		logf("ws client upgrade failed err=%v", err)
		return
	}
	s.serveWSPipe(client, upstream, r)
}

// serveWSPipe 承担一条已建立管道的生命周期：活动状态上报、双向帧
// 搬运、收线。任一侧出错即整体拆除。
func (s *Server) serveWSPipe(client, upstream *websocket.Conn, r *http.Request) {
	setRoute, finish := s.beginRequest()
	session := sessionNameFromHeaders(r.Header)
	started := time.Now()
	logf("ws pipe opened")

	done := make(chan error, 2)
	go func() { done <- relayFrames(upstream, client, nil) }()
	go func() {
		done <- relayFrames(client, upstream, func(model string) {
			// 首帧上报活动状态（与 HTTP native 路径同一入口）。
			setRoute("openai", model, session)
		})
	}()
	err := <-done

	// 尽力让两侧走完 close 握手而不是裸断，然后关 TCP；另一侧的
	// 搬运协程会因连接关闭自然退出。
	closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	deadline := time.Now().Add(time.Second)
	client.WriteControl(websocket.CloseMessage, closeMsg, deadline)
	upstream.WriteControl(websocket.CloseMessage, closeMsg, deadline)
	client.Close()
	upstream.Close()
	<-done

	finish(pipeCloseStatus(err))
	logf("ws pipe closed duration_ms=%d cause=%s",
		time.Since(started).Milliseconds(), pipeCloseCause(err))
}

// relayFrames 单向搬运帧。控制帧（ping/pong/close）由 gorilla 在各自
// 连接上就地处理，不跨侧转发 —— 与通用 WS 代理的 hop-by-hop 语义一致。
// onModel 在首个带 model 字段的文本帧上回调一次。
func relayFrames(src, dst *websocket.Conn, onModel func(string)) error {
	reported := false
	for {
		mtype, data, err := src.ReadMessage()
		if err != nil {
			return err
		}
		if mtype == websocket.TextMessage {
			model := frameModel(data)
			if isRoutedSlug(model) {
				// 本管道只服务原生模型；断线后调用方带 hint 重连
				// 会命中 426 分支，会话自动切 HTTP。
				src.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation,
						"routed model is not served over websocket"),
					time.Now().Add(time.Second))
				return errRoutedFrameOnNativePipe
			}
			if !reported && model != "" && onModel != nil {
				onModel(model)
				reported = true
			}
		}
		if err := dst.WriteMessage(mtype, data); err != nil {
			return err
		}
	}
}

// pipeCloseStatus 把收线错误映射成活动状态码：正常关闭/路由帧哨兵
// → 200；其余（网络断开等）→ 502，让托盘短暂亮错误态。
func pipeCloseStatus(err error) int {
	if errors.Is(err, errRoutedFrameOnNativePipe) {
		return http.StatusOK
	}
	if closeErr, ok := err.(*websocket.CloseError); ok &&
		closeErr.Code == websocket.CloseNormalClosure {
		return http.StatusOK
	}
	return http.StatusBadGateway
}

// pipeCloseCause 生成收线原因日志文本。
func pipeCloseCause(err error) string {
	if err == nil {
		return "closed"
	}
	if errors.Is(err, errRoutedFrameOnNativePipe) {
		return err.Error()
	}
	if closeErr, ok := err.(*websocket.CloseError); ok {
		return "peer close code=" + strconv.Itoa(closeErr.Code) + " text=" + closeErr.Text
	}
	return err.Error()
}

// frameModel 提取帧的 model 字段；解析失败返回空（透传不因畸形帧断线）。
func frameModel(data []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// parseRoutingHintModel 解析 x-codex-routing-hint（"model=slug" 或
// "model=slug;tier=fast"）。
func parseRoutingHintModel(hint string) string {
	for _, part := range strings.Split(hint, ";") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(part), "model="); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// isRoutedSlug：路由模型 slug 一律带 provider 前缀（zai-coding/glm-5.3），
// 原生 slug 从不带 "/"（gpt-5.6-sol、codex-auto-review）。
func isRoutedSlug(model string) bool {
	return model != "" && strings.Contains(model, "/")
}

// wsUpstreamURL 把 native HTTP(S) 端点换成 WS(S)。
func wsUpstreamURL(target string) string {
	switch {
	case strings.HasPrefix(target, "https://"):
		return "wss://" + strings.TrimPrefix(target, "https://")
	case strings.HasPrefix(target, "http://"):
		return "ws://" + strings.TrimPrefix(target, "http://")
	}
	return target
}

// hostOf 提取 URL 的 host 部分用于日志（不含路径）。
func hostOf(target string) string {
	if idx := strings.Index(target, "://"); idx >= 0 {
		rest := target[idx+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			return rest[:slash]
		}
		return rest
	}
	return target
}
