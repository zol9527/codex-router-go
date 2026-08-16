package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 读图执行：单次引擎调用、流中断即失败（已收 delta 丢弃——半份
// 转录会被下游当完整图面引用）。重试、缓存和跨 provider 回退均由
// Codex 调用方决定，Router 只负责把图片转换成一份明确证据或错误。

const (
	responseLimit        = 8 << 20
	defaultVisionTimeout = 120 * time.Second
)

// DescribeCaller 是引擎调用的抽象：registry 引擎走 chat 上游、
// native 走 ChatGPT 后端、local 直连 Ollama。effort 是本次读图的
// 推理档（操作者 pin 优先，否则跟随会话档）。由 server 装配。
type DescribeCaller func(ctx context.Context, engine Engine, effort, question, dataURL string) (string, error)

// Reader 管理一次读图调用。
type Reader struct {
	Caller DescribeCaller
	// Effort 是读图使用的推理档（操作者 pin 优先，否则跟随会话档）。
	Effort string
}

// NewReader 创建读图器。
func NewReader(caller DescribeCaller) *Reader {
	return &Reader{Caller: caller}
}

// Read 返回一张图的证据。每张图只请求当前选定的第一个引擎一次；
// 失败返回错误，由调用方渲染 stated failure。
func (r *Reader) Read(ctx context.Context, engines []Engine, image ImagePart) (Evidence, error) {
	if len(engines) == 0 {
		return Evidence{}, fmt.Errorf("no vision engine configured")
	}
	engine := engines[0]
	transcript, err := r.readOne(ctx, engine, image)
	if err != nil {
		return Evidence{}, fmt.Errorf("%s: %w", engineDisplayName(engine), err)
	}
	return Evidence{
		Engine:     engineDisplayName(engine),
		Question:   image.Question,
		Transcript: transcript,
	}, nil
}

func engineDisplayName(engine Engine) string {
	if engine.DisplayName != "" {
		return engine.DisplayName
	}
	return engine.Slug
}

// readOne 为单次引擎调用提供超时预算；错误不在 Router 内重试。
func (r *Reader) readOne(ctx context.Context, engine Engine, image ImagePart) (string, error) {
	ctxAttempt, cancel := context.WithTimeout(ctx, defaultVisionTimeout)
	defer cancel()
	return r.callEngine(ctxAttempt, engine, image)
}

// transientError 包装状态码，便于调用方保留上游错误语义。
type transientError struct {
	status int
	inner  error
}

func (e *transientError) Error() string {
	if e.inner != nil {
		return fmt.Sprintf("HTTP %d: %v", e.status, e.inner)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

func (e *transientError) Unwrap() error { return e.inner }

// StatusError 暴露状态码。
func StatusError(status int, inner error) error { return &transientError{status: status, inner: inner} }

// StatusErrorWithBody 构造带响应体片段的状态错误：上游 4xx 的拒绝
// 理由必须进日志，否则只剩 "HTTP 400" 的悬案（2026-08-16 视觉桥
// 三引擎全灭事故即因此多查了一轮）。
func StatusErrorWithBody(status int, raw []byte) error {
	snippet := BodySnippet(raw)
	if snippet == "" {
		return StatusError(status, nil)
	}
	return &transientError{status: status, inner: fmt.Errorf("%s", snippet)}
}

// BodySnippet 从错误响应体提取可读片段：优先 error.message /
// error 字符串形态；否则压缩空白后取前 240 字节。空体返回 ""。
func BodySnippet(raw []byte) string {
	var parsed struct {
		Error any `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		switch value := parsed.Error.(type) {
		case string:
			if value != "" {
				return clip(squeeze(value))
			}
		case map[string]any:
			if message, ok := value["message"].(string); ok && message != "" {
				return clip(squeeze(message))
			}
		}
	}
	return clip(squeeze(string(raw)))
}

func squeeze(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func clip(value string) string {
	runes := []rune(value)
	if len(runes) <= 240 {
		return value
	}
	return string(runes[:240]) + "…"
}

// callEngine 通过 Caller 抽象执行一次读图并做响应校验：
// 非空、超限即失败（截断的转录不可信）。
func (r *Reader) callEngine(ctx context.Context, engine Engine, image ImagePart) (string, error) {
	if r.Caller == nil {
		return "", fmt.Errorf("no vision caller configured")
	}
	transcript, err := r.Caller(ctx, engine, r.Effort, image.Question, image.DataURL)
	if err != nil {
		return "", err
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return "", fmt.Errorf("engine returned an empty transcript")
	}
	if len(transcript) > responseLimit {
		return "", fmt.Errorf("engine transcript exceeds the size cap")
	}
	return transcript, nil
}

// ChatDescribeRequest 构造发给 chat 兼容视觉引擎的请求体
// （registry 引擎与本地 Ollama 共用此形态）。
func ChatDescribeRequest(model, question, dataURL string) map[string]any {
	instructions := EvidenceInstructions
	if question != "" {
		instructions += FocusInstructions(question)
	}
	return map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "system", "content": instructions},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Transcribe this image as evidence for a model that cannot see it."},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			}},
		},
		"stream": false,
	}
}

// AnthropicDescribeRequest 构造 anthropic messages 形态的读图请求
//（opencode 的 messages 协议变体引擎）。dataURL 必须是
// data:<media>;base64,<payload> 形态，不合法返回 ok=false。
func AnthropicDescribeRequest(model, question, dataURL string) (map[string]any, bool) {
	mediaType, data, ok := splitDataURL(dataURL)
	if !ok {
		return nil, false
	}
	instructions := EvidenceInstructions
	if question != "" {
		instructions += FocusInstructions(question)
	}
	return map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"system":     instructions,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Transcribe this image as evidence for a model that cannot see it."},
				map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": mediaType, "data": data,
				}},
			}},
		},
	}, true
}

// splitDataURL 拆出 data URL 的 media type 与 base64 载荷。
func splitDataURL(dataURL string) (string, string, bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(dataURL, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	mediaType := strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		return "", "", false
	}
	return mediaType, rest[comma+1:], true
}

// ParseAnthropicDescribeResponse 从 anthropic messages 响应提取文本
//（content 数组的 text block）。
func ParseAnthropicDescribeResponse(raw []byte) (string, error) {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	var texts []string
	for _, block := range parsed.Content {
		if block.Type == "text" && block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	joined := strings.Join(texts, "\n")
	if strings.TrimSpace(joined) == "" {
		return "", fmt.Errorf("no transcript in anthropic engine response")
	}
	return joined, nil
}

// ParseChatDescribeResponse 从非流式 chat 响应提取转录文本。
func ParseChatDescribeResponse(body []byte) (string, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	for _, choice := range parsed.Choices {
		switch content := choice.Message.Content.(type) {
		case string:
			if content != "" {
				return content, nil
			}
		case []any:
			var texts []string
			for _, raw := range content {
				if part, ok := raw.(map[string]any); ok {
					if text, ok := part["text"].(string); ok {
						texts = append(texts, text)
					}
				}
			}
			if joined := strings.Join(texts, ""); joined != "" {
				return joined, nil
			}
		}
	}
	return "", fmt.Errorf("no transcript in engine response")
}

// HTTP helpers shared by callers.

// PostJSON 发一个 JSON POST 并返回状态与字节。
func PostJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body map[string]any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, payload, nil
}
