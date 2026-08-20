// Package responses 是 Responses 直通协议路径：上游原生说 Responses
// （opencode-go-responses 走这里），请求只做 model 还原与路由标记
// 剥除，响应字节原样转发 —— 不经过任何翻译。
package responses

import (
	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/wire"
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

// NeedsResponseTranslation：上游本来就是 Responses —— 直通。
// 直通协议不实现 ResponseTranslator：响应字节原样转发，不存在翻译。
func (Protocol) NeedsResponseTranslation() bool { return false }

func init() {
	wire.Register(Protocol{})
}
