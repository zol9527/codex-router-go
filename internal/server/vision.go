package server

// vision bridge 的 server 集成：引擎候选装配（registry 视觉模型 +
// native GPT + 本地 Ollama）、三路 DescribeCaller、请求管线的图片
// 替换。图片替换发生在协作解密之后、aging 之前。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
		for _, model := range s.readNativeVisionModels() {
			candidates = append(candidates, model)
		}
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

// readNativeVisionModels 从 merged 目录提取 listed 的视觉原生模型。
func (s *Server) readNativeVisionModels() []vision.Engine {
	raw, err := os.ReadFile(filepath.Join(s.opt.State.Dir, "merged-models.json"))
	if err != nil {
		return nil
	}
	var parsed struct {
		Models []struct {
			Slug            string `json:"slug"`
			DisplayName     string `json:"display_name"`
			Priority        any    `json:"priority"`
			Visibility      string `json:"visibility"`
			InputModalities string `json:"input_modalities"`
			Efforts         []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			DefaultEffort string `json:"default_reasoning_level"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	// registry slug 形如 "vendor/model"，native slug 无斜杠。
	registrySlugs := map[string]bool{}
	for _, model := range s.opt.Registry.Models {
		registrySlugs[model.Slug] = true
	}
	var engines []vision.Engine
	for _, model := range parsed.Models {
		if model.Visibility != "list" || registrySlugs[model.Slug] {
			continue
		}
		if !strings.Contains(strings.ToLower(model.InputModalities), "image") {
			continue
		}
		priority := 999
		if p, ok := model.Priority.(float64); ok {
			priority = int(p)
		}
		efforts := make([]string, 0, len(model.Efforts))
		for _, level := range model.Efforts {
			efforts = append(efforts, level.Effort)
		}
		engines = append(engines, vision.Engine{
			Slug: model.Slug, DisplayName: model.DisplayName,
			GatewayModel: model.Slug, Native: true,
			Priority: priority, Efforts: efforts, DefaultEffort: model.DefaultEffort,
			ImageCapable: true,
		})
	}
	return engines
}

// describeCaller 装配三路读图调用。native 路径经闭包捕获原始请求
// （它贡献会话头，而 DescribeCaller 的抽象签名不携带请求）。
func (s *Server) describeCaller(routeModel *registry.Model, r *http.Request) vision.DescribeCaller {
	_ = routeModel
	return func(ctx context.Context, engine vision.Engine, question, dataURL string) (string, error) {
		switch {
		case engine.Local:
			return s.describeLocal(ctx, engine, question, dataURL)
		case engine.Native:
			return s.describeNative(ctx, engine, question, dataURL, r)
		default:
			return s.describeRegistry(ctx, engine, question, dataURL)
		}
	}
}

// describeRegistry：经 chat 上游读图（与普通回合同一凭据/头清洗）。
func (s *Server) describeRegistry(ctx context.Context, engine vision.Engine, question, dataURL string) (string, error) {
	provider := s.opt.Registry.Providers[engine.Provider]
	if provider == nil {
		return "", fmt.Errorf("vision engine provider missing: %s", engine.Provider)
	}
	credential, _ := s.opt.Credentials.Resolve(provider)
	if credential == "" {
		return "", fmt.Errorf("vision engine credential missing: %s", engine.Provider)
	}
	body := vision.ChatDescribeRequest(engine.GatewayModel, question, dataURL)
	headers := translate.UpstreamHeadersFrom(nil, credential, Version)
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "application/json"
	target := strings.TrimSuffix(providerBaseURL(provider), "/") + "/chat/completions"
	status, raw, err := vision.PostJSON(ctx, s.client, target, headers, body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", vision.StatusError(status, nil)
	}
	return vision.ParseChatDescribeResponse(raw)
}

// describeLocal：无凭据直连 Ollama 兼容端点（显式 pin 才会出现）。
func (s *Server) describeLocal(ctx context.Context, engine vision.Engine, question, dataURL string) (string, error) {
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
		return "", vision.StatusError(status, nil)
	}
	return vision.ParseChatDescribeResponse(raw)
}

// describeNative：调用方的活会话 + ChatGPT 后端 /responses 读图。
// 不落任何新凭据；FORWARD_HEADERS 里只有会话头会跟随。
func (s *Server) describeNative(ctx context.Context, engine vision.Engine, question, dataURL string, sourceRequest *http.Request) (string, error) {
	instructions := vision.EvidenceInstructions
	if question != "" {
		instructions += vision.FocusInstructions(question)
	}
	effort := engine.DefaultEffort
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
		"stream": false,
		"store":  false,
	}
	if effort != "" {
		requestBody["reasoning"] = map[string]any{"effort": effort}
	}
	headers := s.nativeHeaders(sourceRequest)
	// native 端点非流式也接受；压缩对大 base64 无益。
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
		return "", vision.StatusError(resp.StatusCode, nil)
	}
	// Responses 非流式：output 数组里 message.content[].text。
	return parseNativeTranscript(payload)
}

// parseNativeTranscript 从 native /responses 非流式体提取文本。
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
