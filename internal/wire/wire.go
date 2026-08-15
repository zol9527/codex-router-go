// Package wire 是协议抽象层：把"Codex 说的 Responses 协议"与
// "上游说的协议"之间的适配，从 server 请求管线里分离出来。
//
// 每条协议路径（chat completions 翻译、Responses 直通、未来的
// anthropic messages 等）实现 Protocol 接口并注册；provider 在注册表
// 里用 Protocol 字段声明自己说哪种协议。server 的路由管线只面向
// wire.Protocol，新增 provider/协议不再触碰请求管线。
//
// 目录结构按协议分家：
//
//	internal/wire/chatcompletion/   Responses ↔ chat completions 翻译
//	internal/wire/responses/        Responses 直通（上游原生说 Responses）
package wire

import (
	"fmt"
	"sort"
	"sync"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/translate"
)

// Request 是 Prepare 的产物：发往上游所需的一切。
type Request struct {
	// Path 是相对 provider base URL 的路径（"/chat/completions"、"/responses"）。
	Path string
	// Body 是已翻译的上游请求体（model 字段已替换为 upstream id）。
	Body map[string]any
	// Stream 记录调用方是否要求流式（协议实现翻译后的真实形态）。
	Stream bool
	// Accept 是响应协商头（"text/event-stream" / "application/json"）。
	Accept string
	// CustomTools 是请求里 custom 工具的名字集合（chat 协议把声明
	// 伪装成 function，响应翻译需要知道哪些名字要还原成
	// custom_tool_call）。非 chat 协议为空。
	CustomTools []string
}

// StreamOptions 携带响应翻译所需的会话上下文。
type StreamOptions struct {
	// SessionModel 是路由会话的模型 slug（create_thread 注入用）。
	SessionModel string
	// EstimateInput 是 prompt-token 补零估算（0 = 不装载）。
	EstimateInput int
	// NamespaceIndex 是本请求的 namespace 还原索引（可空）。
	NamespaceIndex *translate.NamespaceIndex
	// CustomTools 是本请求的 custom 工具名（可空）—— 来自 Prepare
	// 的产物，响应侧据此还原 custom_tool_call 形态。
	CustomTools []string
}

// StreamTranslator 把上游协议的 SSE 流增量翻译成 Responses 事件。
type StreamTranslator interface {
	Created() []byte
	Feed(data string) []byte
	HasContent() bool
	PromptTokens() int64
	OutputTokens() int64
	TotalTokens() int64
	SubstitutedInputTokens() int
}

// Protocol 是一条协议路径的完整适配。
type Protocol interface {
	// Name 是协议注册名（"chat-completions"、"responses"）。
	Name() string
	// Prepare 把 Responses 请求体翻译成上游形态。
	Prepare(responsesRequest map[string]any, model *registry.Model) (*Request, error)
	// NeedsResponseTranslation 报告上游响应是否需要转回 Responses：
	// 直通协议为 false（上游本来就是 Responses，字节原样转发）。
	NeedsResponseTranslation() bool
	// NewStreamTranslator 创建上游 SSE → Responses SSE 的流转换器。
	// 仅 NeedsResponseTranslation 为 true 的协议被调用。
	NewStreamTranslator(model *registry.Model, opts StreamOptions) StreamTranslator
	// TranslateNonStream 把非流式上游响应整体翻译成 Responses JSON。
	TranslateNonStream(upstreamBody map[string]any, model *registry.Model, opts StreamOptions) map[string]any
	// ApplyRequestProfile 施加模型的上游请求画像（effort 阶梯、参数清洗）。
	// Prepare 内部完成 model 替换后调用，职责在协议实现里因为画像
	// 作用于上游协议的字段（chat 的 reasoning_effort / responses 的 reasoning）。
	ApplyRequestProfile(body map[string]any, requestedEffort string, model *registry.Model)
}

// ---- 注册表 ----

var (
	registryMu sync.Mutex
	protocols  = map[string]Protocol{}
)

// Register 登记一个协议实现（重复名后者覆盖，方便测试注入）。
func Register(p Protocol) {
	registryMu.Lock()
	defer registryMu.Unlock()
	protocols[p.Name()] = p
}

// ByName 按注册名取协议。
func ByName(name string) (Protocol, error) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if p, ok := protocols[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no wire protocol registered as %q", name)
}

// ProtocolNameForProvider 把注册表的 provider.Protocol 声明映射到协议名。
// 空声明（绝大多数 provider）= chat completions。
func ProtocolNameForProvider(p *registry.Provider) string {
	if p == nil || p.Protocol == "" {
		return "chat-completions"
	}
	switch p.Protocol {
	case "openai-responses":
		return "responses"
	default:
		return p.Protocol
	}
}

// ForProvider 解析 provider 声明的协议实现。
func ForProvider(p *registry.Provider) (Protocol, error) {
	return ByName(ProtocolNameForProvider(p))
}

// Registered 列出已注册协议名（排序稳定，诊断输出用）。
func Registered() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	names := make([]string, 0, len(protocols))
	for name := range protocols {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
