// Package chatcompletion 是 chat completions 协议路径：
// Codex 的 Responses 请求翻译成 chat completions 发上游，上游的
// chat SSE 增量重组成 Responses 事件流回放。这是替代 LiteLLM 的
// 那条核心路径（zai-coding、opencode-go 走这里）。
package chatcompletion

import (
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/translate"
	"github.com/loyd/codex-router/internal/wire"
)

// Protocol 实现 wire.Protocol。
type Protocol struct{}

// Name 注册名。
func (Protocol) Name() string { return "chat-completions" }

// Prepare：Responses → chat completions 请求翻译。
// 返回的 Body 里 model 已替换为 upstream id、画像已施加。
func (Protocol) Prepare(responsesRequest map[string]any, model *registry.Model) (*wire.Request, error) {
	chat, err := translate.TranslateToChat(responsesRequest)
	if err != nil {
		return nil, err
	}
	// 画像（effort 阶梯、参数清洗）作用在 chat 协议的字段上，
	// 因此在协议实现内完成，server 不感知。
	translate.ApplyRequestProfile(chat.Body, chat.RequestedEffort, model)
	chat.Body["model"] = model.UpstreamModel
	stream := false
	if s, ok := chat.Body["stream"].(bool); ok {
		stream = s
	}
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	return &wire.Request{
		Path:   "/chat/completions",
		Body:   chat.Body,
		Stream: stream,
		Accept: accept,
	}, nil
}

// ApplyRequestProfile 已在 Prepare 内完成；接口方法保留为空操作
// （Prepare 是协议实现的自留地，server 不单独调用）。
func (Protocol) ApplyRequestProfile(_ map[string]any, _ string, _ *registry.Model) {}

// NeedsResponseTranslation：chat 响应必须转回 Responses。
func (Protocol) NeedsResponseTranslation() bool { return true }

// NewStreamTranslator：chat SSE → Responses SSE 的状态机翻译器。
func (Protocol) NewStreamTranslator(_ *registry.Model, opts wire.StreamOptions) wire.StreamTranslator {
	t := translate.NewChatToResponsesSSE("", "").
		WithEstimatedInputTokens(opts.EstimateInput).
		WithNamespaceIndex(opts.NamespaceIndex, opts.SessionModel)
	return t
}

// TranslateNonStream：非流式 chat 响应整体翻译。
func (Protocol) TranslateNonStream(upstreamBody map[string]any, _ *registry.Model, opts wire.StreamOptions) map[string]any {
	t := translate.NewChatToResponsesSSE("", "").
		WithEstimatedInputTokens(opts.EstimateInput).
		WithNamespaceIndex(opts.NamespaceIndex, opts.SessionModel)
	return translate.TranslateNonStreamChatWith(upstreamBody, t)
}

func init() {
	wire.Register(Protocol{})
}
