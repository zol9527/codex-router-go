package server

// vision bridge 的 server 集成：引擎候选装配（registry 视觉模型 +
// native GPT + 本地 Ollama）、三路 DescribeCaller、请求管线的图片
// 替换。图片替换发生在协作解密之后、spill 之前。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/translate"
	"github.com/loyd/codex-router/internal/vision"
)

// visionConcurrency 是单回合内并发读图的软上限
// （操作者要等全部读完才开回合，但引擎是限流账户，不能拿相册轰炸）。
const visionConcurrency = 3

// bridgeVision 对携带图片的 routed 回合执行读图与替换。
// 无图 / 桥关闭 / 无引擎可解析时零成本直通。
func (s *Server) bridgeVision(w http.ResponseWriter, r *http.Request,
	payload map[string]any, routeModel *registry.Model) {

	input, ok := payload["input"].([]any)
	if !ok || !vision.InputHasImage(input) {
		return
	}
	settings, configured := vision.ReadSettings(s.opt.State.Dir)
	if !settings.EffectiveEnabled(configured) {
		return
	}
	engines := vision.ResolveEngines(s.visionCandidates(r), settings, configured)
	if len(engines) == 0 {
		return
	}
	reader := vision.NewReader(s.describeCaller(routeModel, r))
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
	if account := r.Header.Get("Chatgpt-Account-Id"); account != "" {
		reader.Account = account
	}

	images := vision.CollectImages(input)
	evidence := map[string]vision.Evidence{}
	failures := map[string]string{}
	sem := make(chan struct{}, visionConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, image := range images {
		image := image
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			result, err := reader.Read(r.Context(), engines, image)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures[vision.ImageKey(image.DataURL)] = vision.FailureText(
					engines[0].DisplayName, err)
				logf("vision read failed engine-set=%d error=%v", len(engines), err)
				return
			}
			if result.FellBack {
				logf("vision read fellBack=true engine=%s", result.Engine)
			}
			for _, failure := range result.PriorFailures {
				logf("vision engine attempt failed: %s", failure)
			}
			evidence[vision.ImageKey(image.DataURL)] = result
		}()
	}
	wg.Wait()
	payload["input"] = vision.Substitute(input, evidence, failures)
}

// visionCandidates 装配引擎候选：
//   - registry 里启用 + 有凭据 + 声明 image 的模型（按优先级）；
//   - native 候选：调用方带会话时，从 merged 目录里取 listed 的
//     视觉原生模型 —— native 授权的唯一证据是请求手里的活会话，
//     磁盘上的捕获可能已过期（登出后仍能读到文件）。
//
// 候选构建做同步凭据探测（macOS 上每 provider 一次 security spawn），
// 所以只在确定要读图时调用（调用方已做图片预检）。
func (s *Server) visionCandidates(r *http.Request) []vision.Engine {
	var candidates []vision.Engine
	for _, model := range s.opt.Registry.Models {
		if !vision.SupportsImage(model.InputModalities) {
			continue
		}
		if !s.opt.State.ProviderEnabled(model.Provider, s.opt.Registry.CanonicalProviderID) {
			continue
		}
		provider := s.opt.Registry.ProviderFor(model)
		if provider == nil {
			continue
		}
		if credential, _ := s.opt.Credentials.Resolve(provider); credential == "" {
			continue
		}
		efforts := make([]string, 0, len(model.ReasoningLevels))
		for _, level := range model.ReasoningLevels {
			efforts = append(efforts, level.Effort)
		}
		candidates = append(candidates, vision.Engine{
			Slug: model.Slug, DisplayName: model.DisplayName,
			GatewayModel: model.UpstreamModel, Provider: provider.ID,
			Priority: model.Priority, Efforts: efforts,
			DefaultEffort: model.DefaultEffort, ImageCapable: true,
		})
	}
	// native 候选：调用方带上游会话头（Codex 总是带）。
	if s.hasUpstreamAuthorization(r) {
		registrySlugs := map[string]bool{}
		for _, model := range s.opt.Registry.Models {
			registrySlugs[model.Slug] = true
		}
		candidates = append(candidates, vision.NativeEnginesFromCatalogFile(
			filepath.Join(s.opt.State.Dir, "merged-models.json"), registrySlugs)...)
	}
	return candidates
}

func (s *Server) hasUpstreamAuthorization(r *http.Request) bool {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return false
	}
	return !s.isRouterLocalToken(header)
}

// describeCaller 装配三路读图调用。native 路径经闭包捕获原始请求
// （它贡献会话头，而 DescribeCaller 的抽象签名不携带请求）。
func (s *Server) describeCaller(routeModel *registry.Model, r *http.Request) vision.DescribeCaller {
	_ = routeModel
	return func(ctx context.Context, engine vision.Engine, effort, question, dataURL string) (string, error) {
		switch {
		case engine.Local:
			return s.describeLocal(ctx, engine, effort, question, dataURL)
		case engine.Native:
			return s.describeNative(ctx, engine, effort, question, dataURL, r)
		default:
			// anthropic 协议的引擎（opencode 的 messages 变体）走
			// messages 形态；chat 形态发过去只会 404/400。
			if provider := s.opt.Registry.Providers[engine.Provider]; provider != nil && provider.Protocol == "anthropic" {
				return s.describeAnthropic(ctx, engine, question, dataURL)
			}
			return s.describeRegistry(ctx, engine, effort, question, dataURL)
		}
	}
}

// describeAnthropic：anthropic messages 协议引擎读图。x-api-key +
// anthropic-version 头；非流式即可 —— "stream 必须 true" 是 ChatGPT
// 后端的约束，不适用于 opencode 中继。effort 无对应字段，不传。
func (s *Server) describeAnthropic(ctx context.Context, engine vision.Engine, question, dataURL string) (string, error) {
	provider := s.opt.Registry.Providers[engine.Provider]
	if provider == nil {
		return "", fmt.Errorf("vision engine provider missing: %s", engine.Provider)
	}
	credential, _ := s.opt.Credentials.Resolve(provider)
	if credential == "" {
		return "", fmt.Errorf("vision engine credential missing: %s", engine.Provider)
	}
	body, ok := vision.AnthropicDescribeRequest(engine.GatewayModel, question, dataURL)
	if !ok {
		return "", fmt.Errorf("image data URL is not base64 form")
	}
	headers := translate.UpstreamHeadersFrom(nil, credential, Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "application/json"
	headers["x-api-key"] = credential
	headers["anthropic-version"] = "2023-06-01"
	target := strings.TrimSuffix(providerBaseURL(provider), "/") + "/messages"
	status, raw, err := vision.PostJSON(ctx, s.client, target, headers, body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", vision.StatusErrorWithBody(status, raw)
	}
	return vision.ParseAnthropicDescribeResponse(raw)
}

// describeRegistry：经 chat 上游读图（与普通回合同一凭据/头清洗）。
// effort 走标准 reasoning_effort 字段 —— 上游不认时会拒绝或忽略，
// 与路由回合同一契约。
func (s *Server) describeRegistry(ctx context.Context, engine vision.Engine, effort, question, dataURL string) (string, error) {
	provider := s.opt.Registry.Providers[engine.Provider]
	if provider == nil {
		return "", fmt.Errorf("vision engine provider missing: %s", engine.Provider)
	}
	credential, _ := s.opt.Credentials.Resolve(provider)
	if credential == "" {
		return "", fmt.Errorf("vision engine credential missing: %s", engine.Provider)
	}
	body := vision.ChatDescribeRequest(engine.GatewayModel, question, dataURL)
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	headers := translate.UpstreamHeadersFrom(nil, credential, Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "application/json"
	target := strings.TrimSuffix(providerBaseURL(provider), "/") + "/chat/completions"
	status, raw, err := vision.PostJSON(ctx, s.client, target, headers, body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", vision.StatusErrorWithBody(status, raw)
	}
	return vision.ParseChatDescribeResponse(raw)
}

// describeLocal：无凭据直连 Ollama 兼容端点（显式 pin 才会出现）。
func (s *Server) describeLocal(ctx context.Context, engine vision.Engine, effort, question, dataURL string) (string, error) {
	settings, _ := vision.ReadSettings(s.opt.State.Dir)
	base := vision.LocalBaseURLOf(settings)
	body := vision.ChatDescribeRequest(vision.LocalModelOf(settings), question, dataURL)
	headers := map[string]string{"Accept": "application/json"}
	status, raw, err := vision.PostJSON(ctx, s.client, strings.TrimSuffix(base, "/")+"/chat/completions", headers, body)
	if err != nil {
		// 保留传输层自己的措辞 —— 操作者因此知道本地引擎没开。
		return "", err
	}
	if status != http.StatusOK {
		return "", vision.StatusErrorWithBody(status, raw)
	}
	return vision.ParseChatDescribeResponse(raw)
}

// describeNative：调用方的活会话 + ChatGPT 后端 /responses 读图。
// 不落任何新凭据；FORWARD_HEADERS 里只有会话头会跟随。
func (s *Server) describeNative(ctx context.Context, engine vision.Engine, effort, question, dataURL string, sourceRequest *http.Request) (string, error) {
	instructions := vision.EvidenceInstructions
	if question != "" {
		instructions += vision.FocusInstructions(question)
	}
	// 档位优先级：操作者/会话传入的 effort > 引擎默认 > 声明阶梯末档。
	if effort == "" {
		effort = engine.DefaultEffort
	}
	if effort == "" && len(engine.Efforts) > 0 {
		effort = engine.Efforts[len(engine.Efforts)-1]
	}
	requestBody := map[string]any{
		"model":        engine.GatewayModel,
		"instructions": instructions,
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "Transcribe this image as evidence for a model that cannot see it."},
				map[string]any{"type": "input_image", "image_url": dataURL},
			},
		}},
		// 后端强制流式：非流式 400 {"detail":"Stream must be set to true"}
		// （2026-08-16 错误体实锤）。协作载荷中继同款 stream:true 已在生产验证。
		"stream": true,
		"store":  false,
	}
	if effort != "" {
		requestBody["reasoning"] = map[string]any{"effort": effort}
	}
	headers := s.nativeHeaders(sourceRequest)
	headers["Accept"] = "text/event-stream"
	raw, err := json.Marshal(requestBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.nativeTarget("/responses"), strings.NewReader(string(raw)))
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", vision.StatusErrorWithBody(resp.StatusCode, payload)
	}
	return parseNativeTranscriptStream(payload)
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
