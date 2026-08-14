package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 读图执行：一图一购（缓存 + in-flight 共享）、瞬时失败重试、
// 引擎回退、流中断即失败（已收 delta 丢弃——半份转录会被下游
// 当完整图面引用）。

const (
	responseLimit        = 8 << 20
	defaultVisionTimeout = 120 * time.Second
	// 瞬时失败重试：250ms 与 1s 各一次；4xx 的同样拒绝不买任何东西。
	retryDelayFirst  = 250 * time.Millisecond
	retryDelaySecond = 1 * time.Second
)

// DescribeCaller 是引擎调用的抽象：registry 引擎走 chat 上游、
// native 走 ChatGPT 后端、local 直连 Ollama。由 server 装配。
type DescribeCaller func(ctx context.Context, engine Engine, question string, dataURL string) (string, error)

// Reader 管理缓存与 in-flight 共享。
type Reader struct {
	Caller DescribeCaller
	// Effort 是读图使用的推理档（跟随操作者的会话档）。
	Effort string
	// Account 是 native 引擎的账号键（转录按账号隔离）。
	Account string

	mu       sync.Mutex
	cache    map[string]Evidence
	inflight map[string]*inflightRead
}

type inflightRead struct {
	done     chan struct{}
	evidence Evidence
	err      error
}

// NewReader 创建读图器。
func NewReader(caller DescribeCaller) *Reader {
	return &Reader{
		Caller:   caller,
		cache:    map[string]Evidence{},
		inflight: map[string]*inflightRead{},
	}
}

// evidenceLimit 是缓存上限（条目）。
const evidenceLimit = 128

// Read 返回一张图的证据（缓存 → in-flight 共享 → 引擎列表逐个尝试
// + 重试）。失败返回错误 —— 调用方渲染 stated failure。
func (r *Reader) Read(ctx context.Context, engines []Engine, image ImagePart) (Evidence, error) {
	key := CacheKey(image.DataURL, r.Effort, r.Account, image.Question)
	r.mu.Lock()
	if evidence, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return evidence, nil
	}
	if pending, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		select {
		case <-pending.done:
			return pending.evidence, pending.err
		case <-ctx.Done():
			return Evidence{}, ctx.Err()
		}
	}
	pending := &inflightRead{done: make(chan struct{})}
	r.inflight[key] = pending
	r.mu.Unlock()

	defer func() {
		close(pending.done)
		r.mu.Lock()
		delete(r.inflight, key)
		r.mu.Unlock()
	}()

	evidence, err := r.readWithFallback(ctx, engines, image)
	pending.evidence, pending.err = evidence, err
	if err == nil {
		r.mu.Lock()
		if len(r.cache) >= evidenceLimit {
			for k := range r.cache { // 简单驱逐：任一条目
				delete(r.cache, k)
				break
			}
		}
		r.cache[key] = evidence
		r.mu.Unlock()
	}
	return evidence, err
}

// readWithFallback：按引擎列表逐个尝试；每个引擎对瞬时失败重试两次
// （250ms、1s），4xx 不重试；只有当前引擎彻底失败才尝试下一个。
// 回退在证据里标记（fellBack），日志由调用方打印。
func (r *Reader) readWithFallback(ctx context.Context, engines []Engine, image ImagePart) (Evidence, error) {
	var lastErr error
	for index, engine := range engines {
		transcript, err := r.readOneWithRetry(ctx, engine, image)
		if err == nil {
			return Evidence{
				Engine:     engineDisplayName(engine),
				Question:   image.Question,
				Transcript: transcript,
				FellBack:   index > 0,
			}, nil
		}
		lastErr = fmt.Errorf("%s: %w", engineDisplayName(engine), err)
	}
	return Evidence{}, lastErr
}

func engineDisplayName(engine Engine) string {
	if engine.DisplayName != "" {
		return engine.DisplayName
	}
	return engine.Slug
}

// readOneWithRetry：瞬时失败（429/5xx/传输）重试两次；4xx 直接失败；
// 每次尝试带完整超时预算。
func (r *Reader) readOneWithRetry(ctx context.Context, engine Engine, image ImagePart) (string, error) {
	delays := []time.Duration{retryDelayFirst, retryDelaySecond}
	var lastErr error
	for attempt := 0; ; attempt++ {
		ctxAttempt, cancel := context.WithTimeout(ctx, defaultVisionTimeout)
		transcript, err := r.callEngine(ctxAttempt, engine, image)
		cancel()
		if err == nil {
			return transcript, nil
		}
		lastErr = err
		if !isTransientVisionFailure(err) || attempt >= len(delays) {
			return "", lastErr
		}
		select {
		case <-time.After(delays[attempt]):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// transientError 包装状态码供重试判定。
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

// StatusError 暴露状态码（调用方判定重试）。
func StatusError(status int, inner error) error { return &transientError{status: status, inner: inner} }

func isTransientVisionFailure(err error) bool {
	var te *transientError
	if ok := asTransient(err, &te); ok {
		return te.status == 429 || te.status >= 500
	}
	// 传输失败（无状态码）：连接类错误重试值得。
	return err != nil && !strings.Contains(err.Error(), "status 4")
}

func asTransient(err error, target **transientError) bool {
	if te, ok := err.(*transientError); ok {
		*target = te
		return true
	}
	return false
}

// callEngine 通过 Caller 抽象执行一次读图并做响应校验：
// 非空、超限即失败（截断的转录不可信）。
func (r *Reader) callEngine(ctx context.Context, engine Engine, image ImagePart) (string, error) {
	if r.Caller == nil {
		return "", fmt.Errorf("no vision caller configured")
	}
	transcript, err := r.Caller(ctx, engine, image.Question, image.DataURL)
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
