// Package responses 是 Responses 直通协议路径：上游原生说 Responses
// （opencode-go-responses 走这里），请求只做 model 还原与路由标记
// 剥除，响应字节原样转发 —— 不经过任何翻译。
package responses

import (
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/wire"
)

// Protocol 实现 wire.Protocol。
type Protocol struct{}

// Name 注册名。
func (Protocol) Name() string { return "responses" }

// Prepare：model 还原 + Codex 专属标记剥除；协议字段全部保留
// （store/include 等 Responses 端点字段上游自己认得）。
func (Protocol) Prepare(responsesRequest map[string]any, model *registry.Model) (*wire.Request, error) {
	translated := make(map[string]any, len(responsesRequest))
	for k, v := range responsesRequest {
		translated[k] = v
	}
	translated["model"] = model.UpstreamModel
	delete(translated, "client_metadata")
	stream := false
	if s, ok := translated["stream"].(bool); ok {
		stream = s
	}
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	return &wire.Request{
		Path:   "/responses",
		Body:   translated,
		Stream: stream,
		Accept: accept,
	}, nil
}

// ApplyRequestTranslation：直通协议不施加画像（Responses 字段上游
// 原样认得；effort 等控制由 Codex 的 reasoning 对象自带）。
func (Protocol) ApplyRequestProfile(_ map[string]any, _ string, _ *registry.Model) {}

// NeedsResponseTranslation：上游本来就是 Responses —— 直通。
func (Protocol) NeedsResponseTranslation() bool { return false }

// NewStreamTranslator / TranslateNonStream：直通协议不会被调用
// （server 对 NeedsResponseTranslation=false 的响应原样转发）；
// 实现为防御性 panic，误用时立刻暴露而不是悄悄错误翻译。
func (Protocol) NewStreamTranslator(_ *registry.Model, _ wire.StreamOptions) wire.StreamTranslator {
	panic("responses protocol relays bytes verbatim; no stream translation exists")
}

func (Protocol) TranslateNonStream(_ map[string]any, _ *registry.Model, _ wire.StreamOptions) map[string]any {
	panic("responses protocol relays bytes verbatim; no non-stream translation exists")
}

func init() {
	wire.Register(Protocol{})
}
