package responses

import (
	"testing"

	"github.com/loyd/codex-router/internal/domain/registry"
)

// Prepare：model 还原、标记剥除、Responses 字段全保留。
func TestPreparePassthrough(t *testing.T) {
	p := Protocol{}
	model := &registry.Model{
		Slug:          "opencode-go-responses/gpt-5.6-luna",
		UpstreamModel: "gpt-5.6-luna",
	}
	prepared, err := p.Prepare(map[string]any{
		"model":           "opencode-go-responses/gpt-5.6-luna",
		"input":           "hi",
		"stream":          true,
		"store":           false,
		"include":         []any{"reasoning.encrypted_content"},
		"client_metadata": map[string]any{"originator": "codex"},
		"reasoning":       map[string]any{"effort": "high"},
	}, model)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Path != "/responses" {
		t.Errorf("path = %s", prepared.Path)
	}
	if prepared.Body["model"] != "gpt-5.6-luna" {
		t.Errorf("model must be restored: %v", prepared.Body["model"])
	}
	if _, has := prepared.Body["client_metadata"]; has {
		t.Error("caller metadata must be stripped")
	}
	// Responses 专属字段上游认得，全部保留。
	for _, key := range []string{"store", "include", "reasoning"} {
		if _, has := prepared.Body[key]; !has {
			t.Errorf("%s must survive passthrough", key)
		}
	}
	if prepared.Accept != "text/event-stream" {
		t.Errorf("accept = %s", prepared.Accept)
	}
}

// 直通判定。
func TestNeedsNoTranslation(t *testing.T) {
	p := Protocol{}
	if p.NeedsResponseTranslation() {
		t.Error("responses protocol relays verbatim")
	}
}
