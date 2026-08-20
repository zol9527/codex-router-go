package vision

import (
	"context"
	"encoding/json"
	"fmt"
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

// DescribeCaller 是引擎调用的抽象：native 走 ChatGPT 后端。effort
// 是本次读图的推理档（操作者 pin 优先，否则跟随会话档）。由 server
// 装配。
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

// transientError 包装状态码，便于调用方保留上游错误语义。
