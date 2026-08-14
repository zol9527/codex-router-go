package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// compaction 上游 fixture：返回非流式摘要。
func compactionUpstream(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	var seen *map[string]any
	seenPtr := &map[string]any{}
	seen = seenPtr
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		*seen = body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Task summary: halfway done."}}],"usage":{"prompt_tokens":900,"completion_tokens":80,"total_tokens":980}}`)
	}))
	return upstream, seen
}

// v1 /responses/compact：上游收到非流式无工具请求；
// 输出 = 尾预算内 user 消息 + 摘要消息。
func TestCompactionV1(t *testing.T) {
	upstream, seen := compactionUpstream(t)
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses/compact",
		strings.NewReader(`{
			"model": "zai-coding/glm-5.3",
			"input": [
				{"type":"message","role":"user","content":[{"type":"input_text","text":"first task"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"second task"}]}
			],
			"stream": true
		}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// 上游侧：非流式、无工具、末尾是压缩指令。
	if (*seen)["stream"] != false {
		t.Error("compaction must be non-streaming upstream")
	}
	if tools, ok := (*seen)["tools"].([]any); ok && len(tools) != 0 {
		t.Errorf("compaction must send no tools, got %v", tools)
	}
	messages := (*seen)["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	content := last["content"].([]any)
	if !strings.Contains(content[0].(map[string]any)["text"].(string), "CONTEXT CHECKPOINT COMPACTION") {
		t.Error("compaction prompt must be appended as the final message")
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	output := payload["output"].([]any)
	if len(output) != 3 { // first + second + summary
		t.Fatalf("output = %d items, want 3", len(output))
	}
	summaryItem := output[2].(map[string]any)
	summaryText := summaryItem["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(summaryText, "Task summary: halfway done.") ||
		!strings.Contains(summaryText, summaryPrefixText) {
		t.Errorf("summary message wrong: %q", summaryText)
	}
}

// v2 compaction_trigger + stream：合成 SSE（kcr1: base64 摘要）。
func TestCompactionV2Stream(t *testing.T) {
	upstream, _ := compactionUpstream(t)
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{
			"model": "zai-coding/glm-5.3",
			"input": [
				{"type":"message","role":"user","content":[{"type":"input_text","text":"work"}]},
				{"type":"compaction_trigger"}
			],
			"stream": true
		}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	for _, want := range []string{"response.created", "response.output_item.done", "response.completed", "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("v2 SSE missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"type":"compaction"`) {
		t.Errorf("v2 must emit a compaction item:\n%s", body)
	}
	if !strings.Contains(body, "kcr1:") {
		t.Errorf("summary must be kcr1: base64 encoded:\n%s", body)
	}
	// 触发标记不得进上游历史（上游收到的最后消息是压缩指令，无 trigger）。
}

// v2 非流式：compaction item 的 JSON 快照。
func TestCompactionV2NonStream(t *testing.T) {
	upstream, _ := compactionUpstream(t)
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{
			"model": "zai-coding/glm-5.3",
			"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"work"}]},{"type":"compaction_trigger"}],
			"stream": false
		}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "completed" {
		t.Errorf("snapshot status = %v", payload["status"])
	}
	output := payload["output"].([]any)
	item := output[0].(map[string]any)
	if item["type"] != "compaction" {
		t.Errorf("item type = %v", item["type"])
	}
	// kcr1 摘要可解码回原文。
	encoded, _ := item["encrypted_content"].(string)
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encoded, "kcr1:"))
	if err != nil || !strings.Contains(string(decoded), "halfway done") {
		t.Errorf("kcr1 summary must round-trip: %v %q", err, decoded)
	}
}

// 尾预算选择：从最新往回装，超预算的取尾部。
func TestCompactOutputBudget(t *testing.T) {
	input := []any{
		userMessageItem(strings.Repeat("a", 60_000)),
		map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "assistant text 不计入 user 预算"},
		}},
		userMessageItem(strings.Repeat("b", 50_000)),
	}
	output := compactOutput(input, "S")
	if len(output) != 3 { // 截断的首条 + 完整的次条 + 摘要
		t.Fatalf("output = %d, want 3", len(output))
	}
	// 第二条（最新）完整保留。
	second := output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if len(second) != 50_000 {
		t.Errorf("newest message must be kept whole, len=%d", len(second))
	}
	// 第一条只剩预算尾部（80000-50000=30000）。
	first := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if len(first) != 30_000 || !strings.HasPrefix(first, "a") {
		t.Errorf("oldest message must keep its tail within budget, len=%d", len(first))
	}
}
