package chatcompletion

import (
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/wire"
)

func glmModel() *registry.Model {
	return &registry.Model{
		Slug: "zai-coding/glm-5.3", UpstreamModel: "glm-5.3",
		Provider: "zai-coding", RequestProfile: "glm-thinking",
		ReasoningLevels: []registry.ReasoningLevel{
			{Effort: "low"}, {Effort: "high"}, {Effort: "max"},
		},
	}
}

// Prepare：请求翻译 + 画像 + model 替换 + 流式协商。
func TestPrepare(t *testing.T) {
	p := Protocol{}
	prepared, err := p.Prepare(map[string]any{
		"model":        "zai-coding/glm-5.3",
		"instructions": "be brief",
		"input":        []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		"reasoning":    map[string]any{"effort": "max"},
		"stream":       true,
		"store":        false,
	}, glmModel())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Path != "/chat/completions" || !prepared.Stream || prepared.Accept != "text/event-stream" {
		t.Errorf("request shape wrong: %+v", prepared)
	}
	if prepared.Body["model"] != "glm-5.3" {
		t.Errorf("model must be upstream id: %v", prepared.Body["model"])
	}
	if thinking, ok := prepared.Body["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
		t.Errorf("glm-thinking profile must apply: %v", prepared.Body["thinking"])
	}
	if prepared.Body["reasoning_effort"] != "max" {
		t.Errorf("effort clamp: %v", prepared.Body["reasoning_effort"])
	}
	if _, has := prepared.Body["store"]; has {
		t.Error("Responses-only fields must be dropped")
	}
	messages, ok := prepared.Body["messages"].([]map[string]any)
	if !ok || len(messages) == 0 || messages[0]["role"] != "system" {
		t.Errorf("instructions → leading system message: %T %+v", prepared.Body["messages"], prepared.Body["messages"])
	}
}

// 流翻译器：装上 namespace 与补零估算的选项透传。
func TestStreamTranslator(t *testing.T) {
	p := Protocol{}
	translator := p.NewStreamTranslator(glmModel(), wire.StreamOptions{
		SessionModel:  "zai-coding/glm-5.3",
		EstimateInput: 5000,
	})
	out := string(translator.Feed(`{"choices":[{"delta":{"content":"hello"}}]}`))
	out += string(translator.Feed(`{"usage":{"prompt_tokens":0,"completion_tokens":2}}`))
	out += string(translator.Feed(`[DONE]`))
	if !strings.Contains(out, "output_text.delta") {
		t.Errorf("translation missing:\n%s", out)
	}
	if !strings.Contains(out, `"input_tokens":5000`) {
		t.Errorf("zero substitution must carry the estimate:\n%s", out)
	}
	if !translator.HasContent() {
		t.Error("content must be detected")
	}
}

// 非流式整体翻译。
func TestTranslateNonStream(t *testing.T) {
	p := Protocol{}
	response := p.TranslateNonStream(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "answer"}}},
	}, glmModel(), wire.StreamOptions{})
	if response["object"] != "response" {
		t.Errorf("shape wrong: %v", response["object"])
	}
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", output)
	}
	if output[0].(map[string]any)["type"] != "message" {
		t.Error("expected message item")
	}
}
