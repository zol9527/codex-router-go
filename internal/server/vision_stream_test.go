package server

import (
	"strings"
	"testing"
)

// native 代读必须走流式：后端对非流式 400 {"detail":"Stream must be set
// to true"}（2026-08-16 错误体实锤）。解析聚合 output_text delta，
// completed 的完整 output 兜底。
func TestParseNativeTranscriptStream(t *testing.T) {
	sse := "event: response.output_item.added\n" +
		"data: {\"type\":\"response.output_item.added\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"The image shows \"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"a dialog.\"}\n\n" +
		"data: [DONE]\n\n"
	got, err := parseNativeTranscriptStream([]byte(sse))
	if err != nil {
		t.Fatal(err)
	}
	if got != "The image shows a dialog." {
		t.Errorf("delta aggregation wrong: %q", got)
	}

	// 无 delta 时回落 completed 的完整 output。
	completedOnly := "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"content\":[{\"text\":\"from completed\"}]}]}}\n\n"
	got, err = parseNativeTranscriptStream([]byte(completedOnly))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "from completed") {
		t.Errorf("completed fallback wrong: %q", got)
	}

	if _, err := parseNativeTranscriptStream([]byte("data: {\"type\":\"response.created\"}\n\ndata: [DONE]\n\n")); err == nil {
		t.Error("empty stream must error")
	}
}
