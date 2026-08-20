package wire_test

import (
	"testing"

	"github.com/loyd/codex-router/internal/domain/wire"

	"github.com/loyd/codex-router/internal/domain/registry"
	_ "github.com/loyd/codex-router/internal/domain/wire/chatcompletion"
	_ "github.com/loyd/codex-router/internal/domain/wire/responses"
)

// 注册表：两个协议就位。
func TestRegistry(t *testing.T) {
	names := wire.Registered()
	if len(names) != 2 || names[0] != "chat-completions" || names[1] != "responses" {
		t.Fatalf("registered = %v", names)
	}
	if _, err := wire.ByName("chat-completions"); err != nil {
		t.Error(err)
	}
	if _, err := wire.ByName("nope"); err == nil {
		t.Error("unknown protocol must fail")
	}
}

// provider 声明映射：空 = chat completions；openai-responses = responses。
func TestProtocolNameForProvider(t *testing.T) {
	cases := []struct {
		declared string
		want     string
	}{
		{"", "chat-completions"},
		{"openai-responses", "responses"},
	}
	for _, tc := range cases {
		p := &registry.Provider{Protocol: tc.declared}
		if got := wire.ProtocolNameForProvider(p); got != tc.want {
			t.Errorf("declared %q → %q, want %q", tc.declared, got, tc.want)
		}
	}
	if got := wire.ProtocolNameForProvider(nil); got != "chat-completions" {
		t.Errorf("nil provider → %q", got)
	}
}

// ForProvider 端到端解析（含已注册实现）。
func TestForProvider(t *testing.T) {
	chat := &registry.Provider{ID: "zai-coding"}
	proto, err := wire.ForProvider(chat)
	if err != nil || proto.Name() != "chat-completions" {
		t.Fatalf("chat resolve: %v %v", proto, err)
	}
	if !proto.NeedsResponseTranslation() {
		t.Error("chat protocol must translate responses")
	}
	direct := &registry.Provider{ID: "opencode-go-responses", Protocol: "openai-responses"}
	proto, err = wire.ForProvider(direct)
	if err != nil || proto.Name() != "responses" {
		t.Fatalf("responses resolve: %v %v", proto, err)
	}
	if proto.NeedsResponseTranslation() {
		t.Error("responses protocol must relay verbatim")
	}
}
