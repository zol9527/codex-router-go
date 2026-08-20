package nativebackend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/loyd/codex-router/internal/vision"
)

// Describe 调用 native /responses 完成图片转写；请求构造和 SSE 解析
// 是 Native Backend implementation，不泄漏给 routing/server。
func (b *Backend) Describe(ctx context.Context, request DescribeRequest) (string, error) {
	instructions := vision.EvidenceInstructions
	if request.Question != "" {
		instructions += vision.FocusInstructions(request.Question)
	}
	effort := request.Effort
	if effort == "" {
		effort = request.Engine.DefaultEffort
	}
	if effort == "" && len(request.Engine.Efforts) > 0 {
		effort = request.Engine.Efforts[len(request.Engine.Efforts)-1]
	}
	body := map[string]any{
		"model":        request.Engine.GatewayModel,
		"instructions": instructions,
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "Transcribe this image as evidence for a model that cannot see it."},
				map[string]any{"type": "input_image", "image_url": request.DataURL},
			},
		}},
		"stream": true,
		"store":  false,
	}
	if effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	response, err := b.PostResponses(ctx, Request{Body: body, Header: request.Header, MaxBytes: request.MaxBytes})
	if err != nil {
		return "", err
	}
	if response.Status != 200 {
		return "", vision.StatusErrorWithBody(response.Status, response.Body)
	}
	return ParseTranscriptStream(response.Body)
}

// ParseTranscriptStream 从 native SSE 提取文本；delta 优先，completed
// response 作为兜底。
func ParseTranscriptStream(payload []byte) (string, error) {
	var deltas strings.Builder
	completed := json.RawMessage(nil)
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event.Type {
		case "response.output_text.delta":
			deltas.WriteString(event.Delta)
		case "response.completed":
			completed = event.Response
		}
	}
	if text := deltas.String(); strings.TrimSpace(text) != "" {
		return text, nil
	}
	if len(completed) > 0 {
		return parseTranscript(completed)
	}
	return "", fmt.Errorf("native engine returned no transcript")
}

func parseTranscript(payload []byte) (string, error) {
	var parsed struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", err
	}
	var texts []string
	for _, item := range parsed.Output {
		for _, part := range item.Content {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	joined := strings.Join(texts, "\n")
	if strings.TrimSpace(joined) == "" {
		return "", fmt.Errorf("native engine returned no transcript")
	}
	return joined, nil
}
