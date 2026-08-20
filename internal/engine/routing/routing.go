// Package routing 拥有一次 Routing Turn：输入归一化、图片桥、工具与
// namespace 适配、协议调用、失败收尾与 usage 计量。server 只做 HTTP transport。
package routing

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/translate"
	"github.com/loyd/codex-router/internal/domain/usage"
	"github.com/loyd/codex-router/internal/domain/wire"
	_ "github.com/loyd/codex-router/internal/domain/wire/chatcompletion"
	_ "github.com/loyd/codex-router/internal/domain/wire/responses"
	"github.com/loyd/codex-router/internal/lib/httpx"
)

// Sink 是 routing 的响应接缝。实现不得理解业务，只负责提交 HTTP 头、
// 写 JSON/SSE 和 flush。
type Sink interface {
	WriteJSON(status int, payload any)
	SetHeader(name, value string)
	WriteHeaders(status int, header http.Header)
	WriteSSEHeader()
	Write(chunk []byte) error
	Flush()
}

// ErrorTranslator 把上游错误事实转换成调用方可理解的 Responses 错误体。
type ErrorTranslator func(status int, bodyText, modelName, providerName, providerID string, retryAfter int) map[string]any

// VisionBridge 在进入协议翻译前替换图片与视觉证据。
type VisionBridge func(ctx context.Context, header http.Header, payload map[string]any, model *registry.Model)

// Runner 是 Routing Turn 的外部 interface。主回合与压缩共享私有
// implementation，中间阶段不外泄。native 中继与读图桥不在此列：
// 两者都依赖 server 的会话状态（collab 归一化、vision 缓存），由
// NormalizeInput / BridgeVision 注入，routing 不直接持有 nativebackend。
type Runner struct {
	Registry               func() *registry.Registry
	Client                 *http.Client
	Idle                   time.Duration
	Version                string
	Recorder               *usage.Recorder
	RateLimits             *usage.RateLimitStore
	NormalizeInput         func(context.Context, []any) []any
	BridgeVision           VisionBridge
	ProviderBaseURL        func(*registry.Provider) string
	TranslateProviderError ErrorTranslator
	LogTranslationDegraded func(*wire.Request, *registry.Model)
	Logf                   func(string, ...any)
}

// Request 是一次 Routing Turn 的完整输入。
type Request struct {
	Context    context.Context
	Payload    map[string]any
	Model      *registry.Model
	Provider   *registry.Provider
	Credential string
	Sink       Sink
	Header     http.Header
	Session    string
	Started    time.Time
}

var aliveEventPattern = regexp.MustCompile(
	"response\\.(?:reasoning_summary_text|output_text|function_call_arguments)\\.delta")

func (r *Runner) canonicalProviderID(provider *registry.Provider) string {
	if r.Registry != nil {
		if reg := r.Registry(); reg != nil {
			return reg.CanonicalProviderID(provider.ID)
		}
	}
	return provider.ID
}

func (r *Runner) record(event usage.Event) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Record(event)
}

func (r *Runner) baseURL(provider *registry.Provider) string {
	if provider.BaseURLEnv != "" {
		if value := strings.TrimSpace(os.Getenv(provider.BaseURLEnv)); value != "" {
			return value
		}
	}
	if provider.BaseURL != "" {
		return provider.BaseURL
	}
	return "https://api.openai.com/v1"
}

// UpstreamFailure 表示上游返回了可翻译的 HTTP 错误响应。
// Routing 只负责保留事实，具体错误体翻译由 server transport 决定。
type UpstreamFailure struct {
	Status     int
	BodyText   string
	RetryAfter int
}

func (e *UpstreamFailure) Error() string { return fmt.Sprintf("upstream status %d", e.Status) }

// AttemptOutcome 是一次上游流尝试的完整结果。
// Translator 与 Events 供 transport 层决定空补全、截断和 usage 收尾。
type AttemptOutcome struct {
	Translator wire.StreamTranslator
	Events     *translate.OutputBuffer
}

// StreamRelay 实现空补全守卫的 hold/释放语义：在观察到第一个活性事件
// 前缓冲 SSE；一旦确认上游产生有效内容，再一次性提交响应头并直写后续块。
type StreamRelay struct {
	sink      Sink
	buffered  []byte
	relayed   bool
	aliveSeen bool
	writeErr  error
}

// NewStreamRelay 创建一个绑定到 HTTP transport sink 的流守卫。
func NewStreamRelay(sink Sink) *StreamRelay { return &StreamRelay{sink: sink} }

// HeadersWritten 报告响应头是否已经提交。
func (sr *StreamRelay) HeadersWritten() bool { return sr.relayed }

// HasWriteError 报告向调用方 transport 写回时是否发生错误。
func (sr *StreamRelay) HasWriteError() bool { return sr.writeErr != nil }

func (sr *StreamRelay) emit(chunk []byte) {
	if sr.writeErr != nil {
		return
	}
	if sr.relayed {
		if err := sr.sink.Write(chunk); err != nil {
			sr.writeErr = err
			return
		}
		sr.sink.Flush()
		return
	}
	if aliveEventPattern.Match(chunk) {
		sr.aliveSeen = true
		sr.relayed = true
		sr.sink.WriteSSEHeader()
		if err := sr.sink.Write(sr.buffered); err != nil {
			sr.writeErr = err
			return
		}
		if err := sr.sink.Write(chunk); err != nil {
			sr.writeErr = err
			return
		}
		sr.sink.Flush()
		return
	}
	sr.buffered = append(sr.buffered, chunk...)
}

// FinishFlushWith 在流尚未提交时，用给定 payload 完成一次响应。
func (sr *StreamRelay) FinishFlushWith(payload []byte) error {
	if sr.writeErr != nil {
		return sr.writeErr
	}
	if sr.relayed {
		return fmt.Errorf("cannot replace a response head after it was sent")
	}
	sr.relayed = true
	sr.sink.WriteSSEHeader()
	if err := sr.sink.Write(payload); err != nil {
		return err
	}
	sr.sink.Flush()
	return nil
}

// RunAttempt 执行一次上游请求并增量翻译。它是 Routing Turn 与 HTTP
// transport 之间的最小稳定接缝；调用方负责选择协议、错误码和最终 usage。
// proto 必须是翻译协议（wire.ResponseTranslator）—— 直通协议不走流翻译。
func (r *Runner) RunAttempt(ctx context.Context, target string, headers map[string]string,
	body []byte, model *registry.Model, proto wire.ResponseTranslator, opts wire.StreamOptions,
	relay *StreamRelay) (*AttemptOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := httpx.Fetch(ctx, http.MethodPost, target, headers, body, r.Client, r.Idle)
	if err != nil {
		return &AttemptOutcome{}, err
	}
	defer resp.Body.Close()
	if snapshot := usage.ParseRateLimitHeaders(resp.Header, time.Now()); snapshot != nil && r.RateLimits != nil {
		r.RateLimits.Record(model.Provider, snapshot, time.Now())
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		retryAfter := 0
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			fmt.Sscanf(ra, "%d", &retryAfter)
		}
		return &AttemptOutcome{}, &UpstreamFailure{Status: resp.StatusCode, BodyText: string(raw), RetryAfter: retryAfter}
	}
	translator := proto.NewStreamTranslator(model, opts)
	events := &translate.OutputBuffer{}
	created := translator.Created()
	events.Write(created)
	if relay != nil {
		relay.emit(created)
	}
	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	emit := func() {
		if len(dataLines) == 0 {
			return
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if relay != nil {
			if chunk := translator.Feed(data); len(chunk) > 0 {
				relay.emit(chunk)
				events.Write(chunk)
			}
			return
		}
		events.Write(translator.Feed(data))
	}
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
		} else if trimmed == "" {
			emit()
		}
		if readErr != nil {
			emit()
			if !errors.Is(readErr, io.EOF) && ctx.Err() == nil {
				return &AttemptOutcome{Translator: translator, Events: events}, fmt.Errorf("upstream stream ended before completion: %w", readErr)
			}
			if closing := translator.Feed("[DONE]"); len(closing) > 0 {
				events.Write(closing)
				if relay != nil {
					relay.emit(closing)
				}
			}
			return &AttemptOutcome{Translator: translator, Events: events}, nil
		}
	}
}

func errBody(errType, message string) map[string]any {
	return map[string]any{"error": map[string]any{"type": errType, "message": message}}
}
