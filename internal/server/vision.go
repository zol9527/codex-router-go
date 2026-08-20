package server

// vision bridge 的 server 集成：native 引擎候选装配（调用方会话里的
// 视觉原生模型）、native 读图调用、请求管线的图片替换。图片替换发生
// 在协作解密之后、协议翻译之前。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/loyd/codex-router/internal/nativebackend"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/vision"
)

// visionConcurrency 是单回合内并发读图的软上限
// （操作者要等全部读完才开回合，但引擎是限流账户，不能拿相册轰炸）。
const visionConcurrency = 3

// bridgeVision 对携带图片的 routed 回合执行读图与替换。
// 无图 / 桥关闭 / 无引擎可解析时零成本直通。
func (s *Server) bridgeVision(ctx context.Context, header http.Header,
	payload map[string]any, routeModel *registry.Model) {

	input, ok := payload["input"].([]any)
	if !ok || !vision.InputHasImage(input) {
		return
	}
	settings, configured := vision.ReadSettings(s.opt.State.Dir)
	if !settings.EffectiveEnabled(configured) {
		return
	}
	engines := vision.ResolveEngines(s.visionCandidates(header), settings, configured)
	if len(engines) == 0 {
		return
	}
	reader := vision.NewReader(s.describeCaller(routeModel, header))
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			reader.Effort = effort
		}
	}
	// 操作者 pin 的读图档优先于会话档（tray 的 effort 选择存在
	// vision-bridge.json；不 pin 时保持跟随会话的既有行为）。
	if settings.Effort != "" {
		reader.Effort = settings.Effort
	}
	images := vision.CollectImages(input)
	// 会话名是缓存的第一维键；无 X-Codex-Turn-Metadata 的客户端
	// 由缓存自身判空跳过（不查不写，行为与无缓存时一致）。
	session := sessionNameFromHeaders(header)
	evidence := map[string]vision.Evidence{}
	failures := map[string]string{}
	sem := make(chan struct{}, visionConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, image := range images {
		image := image
		key := vision.ImageKey(image.DataURL)
		// 缓存命中：零成本复用首次转写，不调引擎也不占读图并发额度
		// （Codex 每轮重发历史图片，这里是重复读图的主要来源）。
		if cached, ok := s.visionCache.Get(session, key); ok {
			evidence[key] = cached
			logf("vision cache hit session=%s image=%s", session, key)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			result, err := reader.Read(ctx, engines, image)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures[key] = vision.FailureText(
					engines[0].DisplayName, err)
				logf("vision read failed engine=%s error=%v", engines[0].Slug, err)
				return
			}
			evidence[key] = result
			// 只缓存成功结果：失败不进缓存，下轮还有机会重试。
			s.visionCache.Put(session, key, result)
		}()
	}
	wg.Wait()
	payload["input"] = vision.Substitute(input, evidence, failures)
}

// visionCandidates 装配读图引擎候选：只取调用方 ChatGPT 会话可用的
// 视觉原生模型（merged 目录里 listed 的）—— native 授权的唯一证据
// 是请求手里的活会话，磁盘上的捕获可能已过期（登出后仍能读到文件）。
// 读图走 native 单路：不消耗付费 provider 配额，也不做同步凭据探测。
func (s *Server) visionCandidates(header http.Header) []vision.Engine {
	if !s.hasUpstreamAuthorization(header) {
		return nil
	}
	registrySlugs := map[string]bool{}
	for _, model := range s.opt.Registry.Models {
		registrySlugs[model.Slug] = true
	}
	return vision.NativeEnginesFromCatalogFile(
		filepath.Join(s.opt.State.Dir, "merged-models.json"), registrySlugs)
}

func (s *Server) hasUpstreamAuthorization(header http.Header) bool {
	authorization := strings.TrimSpace(header.Get("Authorization"))
	if authorization == "" {
		return false
	}
	return !s.isRouterLocalToken(authorization)
}

// describeCaller 装配读图调用：native 单路。native 路径经闭包捕获
// 原始请求（它贡献会话头，而 DescribeCaller 的抽象签名不携带请求）。
func (s *Server) describeCaller(routeModel *registry.Model, header http.Header) vision.DescribeCaller {
	_ = routeModel
	return func(ctx context.Context, engine vision.Engine, effort, question, dataURL string) (string, error) {
		return s.describeNative(ctx, engine, effort, question, dataURL, header)
	}
}

// describeNative：调用方的活会话 + ChatGPT 后端 /responses 读图。
// 不落任何新凭据；FORWARD_HEADERS 里只有会话头会跟随。
func (s *Server) describeNative(ctx context.Context, engine vision.Engine, effort, question, dataURL string, header http.Header) (string, error) {
	// 档位选择、请求构造与 SSE 解析都归 Native Backend；server 只传入
	// 调用方会话头，避免 vision 继续感知 endpoint/auth 细节。
	return s.native.Describe(ctx, nativebackend.DescribeRequest{
		Engine: engine, Effort: effort, Question: question, DataURL: dataURL,
		Header: header, MaxBytes: 8 << 20,
	})
}

// parseNativeTranscriptStream 从 native /responses 的 SSE 流提取文本：
// 聚合 response.output_text.delta；completed 事件携带的完整 output
// 作兜底（两种形态都在流里，先到先用）。
func parseNativeTranscriptStream(payload []byte) (string, error) {
	var deltas strings.Builder
	completed := json.RawMessage(nil)
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event.Type {
		case "response.output_text.delta":
			deltas.WriteString(event.Delta)
		case "response.completed":
			completed = event.Response
		}
	}
	if text := deltas.String(); strings.TrimSpace(text) != "" {
		return text, nil
	}
	if len(completed) > 0 {
		return parseNativeTranscript(completed)
	}
	return "", fmt.Errorf("native engine returned no transcript")
}

// parseNativeTranscript 从 native /responses 响应对象提取文本
// （completed 事件的 response 兜底路径）。
func parseNativeTranscript(payload []byte) (string, error) {
	var parsed struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", err
	}
	var texts []string
	for _, item := range parsed.Output {
		for _, part := range item.Content {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	joined := strings.Join(texts, "\n")
	if strings.TrimSpace(joined) == "" {
		return "", fmt.Errorf("native engine returned no transcript")
	}
	return joined, nil
}
