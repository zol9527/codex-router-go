package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// probe 探针的核心判定：mock 一个"丢 arguments、镜像可见"的上游
// （复刻 2026-08-16 的 Z.ai 行为），断言 probeArgumentsVisibility
// 的两形态判定与 probeUsageAccounting 的口径判定。
func TestProbeArgumentsVisibility(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role      string `json:"role"`
				Content   any    `json:"content"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		// 复刻事故上游：只看 tool content（镜像形态），tool_calls 的
		// arguments 字段视而不见；模型回答基于 content 里有无密码。
		visible := false
		for _, m := range body.Messages {
			if m.Role != "tool" {
				continue
			}
			if s, ok := m.Content.(string); ok && strings.Contains(s, probeNeedle) {
				visible = true
			}
		}
		reply := "I don't have visibility into the command that was executed."
		if visible {
			reply = probeNeedle
		}
		// usage 口径：prompt_tokens 不随 arguments 增长（事故口径）。
		var argsLen int
		for _, m := range body.Messages {
			for _, c := range m.ToolCalls {
				argsLen = len(c.Function.Arguments)
			}
		}
		promptTokens := 60
		if argsLen > 10_000 {
			promptTokens = 61 // 字段级不计入：大参数几乎零增量。
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":16}}`, reply, promptTokens)
	}))
	defer upstream.Close()

	client := &http.Client{}
	raw, mirror, rawDetail, mirrorDetail, err := probeArgumentsVisibility(client, upstream.URL, "k", "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if raw {
		t.Errorf("raw form must be invisible on arguments-dropping upstream, got detail %q", rawDetail)
	}
	if !mirror {
		t.Errorf("mirrored form must be visible, got detail %q", mirrorDetail)
	}

	small, big, err := probeUsageAccounting(client, upstream.URL, "k", "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if big > small+500 {
		t.Errorf("mock upstream does not track args in usage; expected frozen accounting, got small=%d big=%d", small, big)
	}
}

// 正常上游（arguments 可见 + usage 随内容增长）的对照面。
func TestProbeOnHealthyUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role      string `json:"role"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		var argsLen int
		for _, m := range body.Messages {
			for _, c := range m.ToolCalls {
				argsLen = len(c.Function.Arguments)
			}
		}
		reply := "not visible"
		if argsLen > 0 {
			reply = probeNeedle // 正常上游看得见 arguments。
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":16}}`,
			reply, 60+argsLen/4)
	}))
	defer upstream.Close()

	client := &http.Client{}
	raw, _, _, _, err := probeArgumentsVisibility(client, upstream.URL, "k", "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if !raw {
		t.Error("healthy upstream must show raw arguments")
	}
	small, big, err := probeUsageAccounting(client, upstream.URL, "k", "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	if big <= small+500 {
		t.Errorf("healthy upstream usage must grow with args, got small=%d big=%d", small, big)
	}
}
