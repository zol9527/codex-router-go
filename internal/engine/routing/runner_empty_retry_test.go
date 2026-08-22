package routing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/usage"
)

// sseSink 收集 runStream 写回的 SSE 流与 JSON 错误（测试用）。
type sseSink struct {
	sse     bool
	status  int
	payload map[string]any
	written []byte
}

func (s *sseSink) WriteJSON(status int, payload any) {
	s.status = status
	raw, _ := json.Marshal(payload)
	_ = json.Unmarshal(raw, &s.payload)
}
func (s *sseSink) SetHeader(string, string)      {}
func (s *sseSink) WriteHeaders(int, http.Header) {}
func (s *sseSink) WriteSSEHeader()               { s.sse = true; s.status = 200 }
func (s *sseSink) Write(b []byte) error          { s.written = append(s.written, b...); return nil }
func (s *sseSink) Flush()                        {}

// emptyRetryFixture 组装一次可重试的流式请求：chat 协议 provider +
// 计数上游。返回 sink、usage 行与上游调用计数。
type emptyRetryFixture struct {
	sink   *sseSink
	runner *Runner
	dir    string
	calls  *int32
}

// newEmptyRetryFixture 先建计数器再接上游 handler：handler 闭包捕获
// 计数器本体（而非尚未赋值的 fixture），按当前调用数切换响应脚本。
func newEmptyRetryFixture(t *testing.T, retryOn bool, script func(w http.ResponseWriter, call int32)) *emptyRetryFixture {
	t.Helper()
	calls := new(int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		script(w, atomic.AddInt32(calls, 1))
	}))
	t.Cleanup(server.Close)

	sink := &sseSink{}
	dir := t.TempDir()
	runner := &Runner{
		Client:   server.Client(),
		Recorder: usage.NewRecorder(dir),
		ProviderBaseURL: func(*registry.Provider) string { return server.URL },
	}
	if retryOn {
		runner.RetryEmptyCompletion = func(*registry.Provider) bool { return true }
	}
	return &emptyRetryFixture{sink: sink, runner: runner, dir: dir, calls: calls}
}

func (f *emptyRetryFixture) run(t *testing.T) {
	t.Helper()
	f.runner.Run(Request{
		Context:    t.Context(),
		Payload: map[string]any{
			"stream": true,
			"input": []any{map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hi"}},
			}},
		},
		Model:      &registry.Model{Slug: "test/model", Provider: "test", UpstreamModel: "up-id"},
		Provider:   &registry.Provider{ID: "test", Kind: "openai-compatible"},
		Credential: "test-key",
		Sink:       f.sink,
		RequestID:  "req-test",
	})
}

// usageRows 读回本次测试写入的计量行。
func (f *emptyRetryFixture) usageRows(t *testing.T) []usage.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "usage-events.jsonl"))
	if err != nil {
		t.Fatalf("read usage events: %v", err)
	}
	var rows []usage.Event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var row usage.Event
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse usage row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// 上游聊天 SSE 片段：空补全（只有 finish）与有内容两种。
func writeChatStream(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	if content != "" {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", content)
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// 场景 1：首轮空补全，重试拿到内容 —— 调用方看到正常流；计量两条
//（首次空补全审计行 emptyRetry=true + 成功行），上游共两次调用。
func TestEmptyCompletionRetrySucceeds(t *testing.T) {
	f := newEmptyRetryFixture(t, true, func(w http.ResponseWriter, call int32) {
		if call == 1 {
			writeChatStream(w, "") // 首轮：空补全
			return
		}
		writeChatStream(w, "hello") // 重试：有内容
	})
	f.run(t)

	if got := atomic.LoadInt32(f.calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if !f.sink.sse || !strings.Contains(string(f.sink.written), "response.output_text.delta") {
		t.Fatalf("client should receive a live stream with content, sse=%v written=%s", f.sink.sse, f.sink.written)
	}
	if strings.Contains(string(f.sink.written), "empty_completion") {
		t.Fatalf("successful retry must hide the empty completion from the client: %s", f.sink.written)
	}
	rows := f.usageRows(t)
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].Status != 502 || !rows[0].EmptyCompletion || !rows[0].EmptyRetry {
		t.Fatalf("first row should be the audited empty attempt: %+v", rows[0])
	}
	if rows[1].Status != 200 || rows[1].EmptyRetry {
		t.Fatalf("second row should be a clean success: %+v", rows[1])
	}
}

// 场景 2：重试仍空 —— 调用方看到 empty_completion 收尾；计量两条
//（审计行 + 最终失败行），上游共两次调用。
func TestEmptyCompletionRetryStillEmpty(t *testing.T) {
	f := newEmptyRetryFixture(t, true, func(w http.ResponseWriter, call int32) {
		writeChatStream(w, "")
	})
	f.run(t)

	if got := atomic.LoadInt32(f.calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if !strings.Contains(string(f.sink.written), "empty_completion") {
		t.Fatalf("client should see the empty_completion failure: %s", f.sink.written)
	}
	rows := f.usageRows(t)
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[0].Status != 502 || !rows[0].EmptyRetry {
		t.Fatalf("first row should be the audited empty attempt: %+v", rows[0])
	}
	if rows[1].Status != 502 || !rows[1].EmptyCompletion || rows[1].EmptyRetry {
		t.Fatalf("final row should be the client-visible failure without retry marker: %+v", rows[1])
	}
}

// 场景 3：开关未开（默认）—— 行为与历史一致：单次上游调用、单条
// 失败行（无 emptyRetry 标记）。
func TestEmptyCompletionNoRetryByDefault(t *testing.T) {
	f := newEmptyRetryFixture(t, false, func(w http.ResponseWriter, call int32) {
		writeChatStream(w, "")
	})
	f.run(t)

	if got := atomic.LoadInt32(f.calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (retry disabled)", got)
	}
	if !strings.Contains(string(f.sink.written), "empty_completion") {
		t.Fatalf("client should see the empty_completion failure: %s", f.sink.written)
	}
	rows := f.usageRows(t)
	if len(rows) != 1 || rows[0].Status != 502 || rows[0].EmptyRetry {
		t.Fatalf("single non-retried failure row expected: %+v", rows)
	}
}

// 场景 4：重试撞上上游 HTTP 错误 —— 如实转发该错误（调用方看到 429
// JSON 而非空补全），计量两条（审计行 + 429 行）。
func TestEmptyCompletionRetryHitsUpstreamError(t *testing.T) {
	f := newEmptyRetryFixture(t, true, func(w http.ResponseWriter, call int32) {
		if call == 1 {
			writeChatStream(w, "")
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"busy"}}`)
	})
	f.run(t)

	if got := atomic.LoadInt32(f.calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if f.sink.status != http.StatusTooManyRequests {
		t.Fatalf("client status = %d, want 429 (retry error must surface)", f.sink.status)
	}
	rows := f.usageRows(t)
	if len(rows) != 2 || rows[0].Status != 502 || !rows[0].EmptyRetry || rows[1].Status != 429 {
		t.Fatalf("usage rows mismatch: %+v", rows)
	}
}
