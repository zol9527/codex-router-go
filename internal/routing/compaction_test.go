package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/wire"
	_ "github.com/loyd/codex-router/internal/wire/responses"
)

// capturingSink 收集 RunCompaction 写回的 JSON（测试用）。
type capturingSink struct {
	status  int
	payload map[string]any
}

func (s *capturingSink) WriteJSON(status int, payload any) {
	s.status = status
	raw, _ := json.Marshal(payload)
	_ = json.Unmarshal(raw, &s.payload)
}
func (s *capturingSink) SetHeader(string, string)      {}
func (s *capturingSink) WriteHeaders(int, http.Header) {}
func (s *capturingSink) WriteSSEHeader()               {}
func (s *capturingSink) Write([]byte) error            { return nil }
func (s *capturingSink) Flush()                        {}

// 直通（responses）协议的 compaction 不得 panic：上游响应本来就是
// Responses 形状，直接提取摘要。旧实现无条件调用翻译方法，直通
// provider 一配即炸（接口拆分修复的回归钉）。
func TestRunCompactionPassthroughProtocol(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"handoff summary"}]}],"usage":{"input_tokens":11,"output_tokens":7}}`)
	}))
	defer upstream.Close()

	model := &registry.Model{Slug: "test/passthrough", Provider: "test", UpstreamModel: "upstream-id"}
	provider := &registry.Provider{ID: "test", Protocol: "openai-responses"}
	sink := &capturingSink{}
	runner := &Runner{
		Client:          upstream.Client(),
		ProviderBaseURL: func(*registry.Provider) string { return upstream.URL },
	}
	result, ok := runner.RunCompaction(Request{
		Context: context.Background(),
		Payload: map[string]any{
			"model":  model.Slug,
			"input":  []any{map[string]any{"type": "message", "role": "user"}},
			"stream": false,
		},
		Model: model, Provider: provider, Credential: "key", Sink: sink,
	})
	if !ok {
		t.Fatalf("compaction failed: status=%d payload=%v", sink.status, sink.payload)
	}
	if result.Summary != "handoff summary" {
		t.Fatalf("summary = %q, want handoff summary", result.Summary)
	}
	if result.PromptTokens != 11 || result.CompletionTokens != 7 {
		t.Fatalf("usage = %+v, want 11/7", result)
	}
}

// 翻译契约：NeedsResponseTranslation=true 的协议必须实现
// wire.ResponseTranslator，违反时 TranslatorFor 显式报错。
type lyingProtocol struct{}

func (lyingProtocol) Name() string                                    { return "lying" }
func (lyingProtocol) Prepare(map[string]any, *registry.Model) (*wire.Request, error) {
	return &wire.Request{Path: "/x", Body: map[string]any{}}, nil
}
func (lyingProtocol) NeedsResponseTranslation() bool { return true }

func TestTranslatorForRejectsLyingProtocol(t *testing.T) {
	if _, err := wire.TranslatorFor(lyingProtocol{}); err == nil {
		t.Fatal("TranslatorFor must reject a translation-needing protocol without ResponseTranslator")
	} else if !strings.Contains(err.Error(), "lying") {
		t.Fatalf("error should name the protocol: %v", err)
	}
}
