package vision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 分层进度求和：与 Node 版一致 —— 每层的 completed 只前进（新层
// completed 从零累计），总体百分比在新层出现时会被稀释回落，
// 收敛到 100。层内绝不倒退。
func TestProgressTrackerMonotonic(t *testing.T) {
	tracker := NewProgressTracker()
	events := []map[string]any{
		{"digest": "layer-a", "status": "pulling", "completed": 50.0, "total": 100.0},
		{"digest": "layer-a", "status": "pulling", "completed": 100.0, "total": 100.0},
		// 第二层开始：completed 回到 0，但总和 = 100 + 0。
		{"digest": "layer-b", "status": "pulling", "completed": 0.0, "total": 100.0},
		{"digest": "layer-b", "status": "pulling", "completed": 100.0, "total": 100.0},
	}
	percents := make([]int, 0, len(events))
	for _, event := range events {
		percents = append(percents, tracker.Update(event))
	}
	// 最终 100%；同一层重复报告时（第三、四个事件之间 completed
	// 只增）百分比不倒退。
	if percents[len(percents)-1] != 100 {
		t.Errorf("final percent = %d, want 100", percents[len(percents)-1])
	}
	if percents[0] > percents[1] {
		t.Errorf("within a layer percent regressed: %v", percents)
	}
	// 新层稀释后恢复：50 → 100 → 50 → 100 的收敛形状。
	if percents[1] != 100 || percents[3] != 100 {
		t.Errorf("layer completion must reach 100: %v", percents)
	}
	// 无数字的事件返回 -1。
	if got := tracker.Update(map[string]any{"status": "verifying"}); got != -1 {
		t.Errorf("no-numbers event = %d, want -1", got)
	}
}

// pull 流：NDJSON 事件 → 进度回调；error 事件失败；缺 success 失败。
func TestStreamOllamaPull(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			t.Errorf("pull path = %s (must be on daemon root, not /v1)", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"status":"pulling","digest":"sha256:aaa","completed":50,"total":100}`+"\n")
		fmt.Fprint(w, `{"status":"pulling","digest":"sha256:aaa","completed":100,"total":100}`+"\n")
		fmt.Fprint(w, `{"status":"success"}`+"\n")
	}))
	defer upstream.Close()

	var percents []int
	err := StreamOllamaPull(context.Background(), nil, "qwen2.5vl:3b", upstream.URL+"/v1",
		func(detail string, percent int) { percents = append(percents, percent) })
	if err != nil {
		t.Fatal(err)
	}
	if len(percents) == 0 || percents[len(percents)-1] != 100 {
		t.Errorf("progress callbacks wrong: %v", percents)
	}

	// error 事件 → 失败。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":"manifest unknown"}`+"\n")
	}))
	defer failing.Close()
	if err := StreamOllamaPull(context.Background(), nil, "x", failing.URL, nil); err == nil ||
		!strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("error event must fail: %v", err)
	}

	// 无 success → 失败。
	incomplete := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"pulling","digest":"a","completed":1,"total":2}`+"\n")
	}))
	defer incomplete.Close()
	if err := StreamOllamaPull(context.Background(), nil, "x", incomplete.URL, nil); err == nil {
		t.Error("missing success must fail")
	}

	// 不可达 → 友好错误。
	if err := StreamOllamaPull(context.Background(), nil, "x", "http://127.0.0.1:1", nil); err == nil ||
		!strings.Contains(err.Error(), "Is it installed and running") {
		t.Errorf("unreachable message wrong: %v", err)
	}
}

// detached pull 全流程：进度文件 → done → adopt 语义（无引擎时才采用）。
func TestRunDetachedPull(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, `{"status":"pulling","digest":"a","completed":100,"total":100}`+"\n")
		fmt.Fprint(w, `{"status":"success"}`+"\n")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	// 无任何桥配置 → 下载后采用为首引擎。
	if err := RunDetachedPull(dir, "qwen2.5vl:3b", upstream.URL); err != nil {
		t.Fatal(err)
	}
	download := ReadDownload(dir)
	if download == nil || download.Status != "done" || download.Percent != 100 || !download.Adopted {
		t.Fatalf("download state wrong: %+v", download)
	}
	settings, configured := ReadSettings(dir)
	if !configured || settings.Engine != LocalEngineSlug || settings.LocalModel != "qwen2.5vl:3b" {
		t.Errorf("first download must be adopted: %+v configured=%v", settings, configured)
	}

	// 已启用且已有引擎 → 下载不采用（下载不是选择：新模型未测量，
	// 悄悄顶掉已知可靠的读图器是把准确转录换成可能编造）。
	dir2 := t.TempDir()
	enabled := true
	raw, _ := json.Marshal(Settings{Enabled: &enabled, Engine: "zai/glm-v"})
	os.WriteFile(filepath.Join(dir2, "vision-bridge.json"), raw, 0o600)
	if err := RunDetachedPull(dir2, "llava", upstream.URL); err != nil {
		t.Fatal(err)
	}
	if download := ReadDownload(dir2); download.Adopted {
		t.Error("download must not adopt over an active engine choice")
	}
	settings2, _ := ReadSettings(dir2)
	if settings2.Engine != "zai/glm-v" || !settings2.EffectiveEnabled(true) {
		t.Errorf("active engine choice must be untouched: %+v", settings2)
	}

	// 失败下载：error 状态 + 不采用。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":"boom"}`+"\n")
	}))
	defer failing.Close()
	dir3 := t.TempDir()
	if err := RunDetachedPull(dir3, "x", failing.URL); err == nil {
		t.Fatal("expected failure")
	}
	if download := ReadDownload(dir3); download == nil || download.Status != "error" || download.Error == "" {
		t.Errorf("failure must be recorded: %+v", download)
	}
	if _, configured := ReadSettings(dir3); configured {
		t.Error("failed download must not touch the engine choice")
	}
}

// 目录排序：measured-accurate 优先、untested 中间、captions-only 垫底，
// 同档小下载优先 —— picker 永不把自信-读错放最上。
func TestRankedLocalVision(t *testing.T) {
	ranked := RankedLocalVision()
	if ranked[0].Tag != "qwen2.5vl:3b" {
		t.Errorf("measured accurate model must rank first, got %s", ranked[0].Tag)
	}
	last := ranked[len(ranked)-1]
	if last.Accuracy != "captions-only" || last.Measured == nil || last.Measured.Percent != 0 {
		t.Errorf("zero-scoring captioner must rank last: %+v", last)
	}
	// unmeasured 的档位只能是 untested。
	for _, model := range ranked {
		if model.Measured == nil && model.Accuracy != "untested" {
			t.Errorf("unmeasured model %s claims %s", model.Tag, model.Accuracy)
		}
	}
}

// 进程身份：身份与 PID 同时匹配才算拥有。
func TestStateOwnsProcess(t *testing.T) {
	// 当前进程：真实身份。
	self := os.Getpid()
	identity := ProcessStartIdentity(self)
	if identity == "" {
		t.Skip("process identity unavailable on this platform")
	}
	state := &RuntimeState{Version: 1, Managed: true, PID: self, ProcessStart: identity}
	if !StateOwnsProcess(state) {
		t.Error("matching identity must own")
	}
	// 身份不匹配（PID 复用形态）。
	state.ProcessStart = "fake-start-time"
	if StateOwnsProcess(state) {
		t.Error("mismatched identity must not own")
	}
	// 不存在的 PID。
	state2 := &RuntimeState{Version: 1, Managed: true, PID: 999999, ProcessStart: "x"}
	if StateOwnsProcess(state2) {
		t.Error("dead pid must not own")
	}
}

// 探测：/models 列表与视觉过滤；不可达的错误形态。
func TestProbeLocalServer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe path = %s (probe uses the OpenAI-compatible /v1 face)", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"qwen2.5vl:3b"},{"id":"llama3:8b"}]}`)
	}))
	defer upstream.Close()
	probe := ProbeLocalServer(context.Background(), nil, upstream.URL+"/v1")
	if !probe.Reachable {
		t.Fatalf("probe failed: %s", probe.Error)
	}
	if len(probe.Models) != 2 || len(probe.VisionModels) != 1 || probe.VisionModels[0] != "qwen2.5vl:3b" {
		t.Errorf("models wrong: %+v", probe)
	}

	dead := ProbeLocalServer(context.Background(), nil, "http://127.0.0.1:1")
	if dead.Reachable || dead.Error == "" {
		t.Errorf("dead server must report error: %+v", dead)
	}
}

// 运行时状态往返。
func TestRuntimeStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if ReadRuntimeState(dir) != nil {
		t.Error("missing state reads nil")
	}
	os.WriteFile(filepath.Join(dir, "ollama-runtime.json"),
		[]byte(`{"version":1,"managed":true,"pid":123,"processIdentity":"ps-identity"}`), 0o600)
	state := ReadRuntimeState(dir)
	if state == nil || !state.Managed || state.PID != 123 {
		t.Fatalf("round trip failed: %+v", state)
	}
	// PID 123 不会是我们的身份 → 不拥有。
	if StateOwnsProcess(state) {
		t.Error("stale pid+identity must not own")
	}
}

var _ = time.Now
