package vision

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// 门控语义：无文件=默认开；enabled=false=永远关；解析失败=关。
func TestStateGating(t *testing.T) {
	dir := t.TempDir()
	settings, configured := ReadSettings(dir)
	if configured {
		t.Error("missing file means never configured")
	}
	if !settings.EffectiveEnabled(configured) {
		t.Error("default is on")
	}
	writeFile(t, dir, "vision-bridge.json", `{"enabled": false}`)
	settings, configured = ReadSettings(dir)
	if !configured || settings.EffectiveEnabled(configured) {
		t.Error("stored false must stay off forever")
	}
	writeFile(t, dir, "vision-bridge.json", `{"enabled": true}`)
	settings, configured = ReadSettings(dir)
	if !configured || !settings.EffectiveEnabled(configured) {
		t.Error("stored true must stay on")
	}
	writeFile(t, dir, "vision-bridge.json", `{corrupt`)
	settings, configured = ReadSettings(dir)
	if !configured {
		t.Error("file exists → configured")
	}
	if settings.EffectiveEnabled(configured) {
		t.Error("unreadable file falls back to off, not the default")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 引擎解析：pin 优先、pin 失效（显式）返回空、auto 跳过 loopback，
// 且始终只选择一个引擎。
func TestResolveEngines(t *testing.T) {
	candidates := []Engine{
		{Slug: "oc/gpt-v", DisplayName: "GPT Vision", GatewayModel: "gpt-v", Native: true, Priority: 20, ImageCapable: true},
		{Slug: "gpt-5.6-luna", DisplayName: "Luna", Native: true, Priority: 5, ImageCapable: true},
		{Slug: "oc/claude-v", DisplayName: "Claude Vision", Native: true, Priority: 30, ImageCapable: true},
	}
	enabled := true
	on := Settings{Enabled: &enabled}

	// 排序取首位：priority 小者优先（luna=5）。
	engines := ResolveEngines(candidates, on, false)
	if len(engines) != 1 || engines[0].Slug != "gpt-5.6-luna" {
		t.Fatalf("resolution must take the ranked head: %+v", engines)
	}
	// 无候选 → 空。
	if engines := ResolveEngines(nil, on, false); len(engines) != 0 {
		t.Errorf("no candidates must resolve nothing, got %+v", engines)
	}
	// 关闭 → 空。
	off := Settings{Enabled: &[]bool{false}[0]}
	if engines := ResolveEngines(candidates, off, true); len(engines) != 0 {
		t.Errorf("disabled bridge must resolve nothing")
	}
}

// 输入扫描：user 消息的 input_image 与工具结果里的 data: URL 都提取；
// 最新问题跟随；<image path> 标注提取。
func TestCollectImages(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "first question"},
		}},
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "<image path=\"/tmp/shot.png\">"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		}},
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "what does this error say?"},
		}},
		map[string]any{"type": "function_call_output", "call_id": "c1",
			"output": `{"type":"input_image","image_url":"data:image/png;base64,BBBB"}`},
	}
	if !InputHasImage(input) {
		t.Fatal("input has images")
	}
	images := CollectImages(input)
	if len(images) != 2 {
		t.Fatalf("images = %d, want 2", len(images))
	}
	if images[0].DataURL != "data:image/png;base64,AAAA" || images[0].FilePath != "/tmp/shot.png" {
		t.Errorf("first image wrong: %+v", images[0])
	}
	// 最新问题跟随（最新的 user 提问）。
	if images[0].Question != "what does this error say?" {
		t.Errorf("question must follow the latest ask: %q", images[0].Question)
	}
	// 工具结果里的 data URL。
	if !strings.Contains(images[1].DataURL, "BBBB") {
		t.Errorf("tool output image missing: %+v", images[1])
	}
}

// 问题清洗：剥 <image> 包装与上下文块。
func TestCleanQuestion(t *testing.T) {
	item := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": "# Files mentioned by the user:\n- /a.png\n\n<image path=\"/a.png\">"},
		map[string]any{"type": "input_text", "text": "why is this red?"},
	}}
	if got := messageQuestion(item); got != "why is this red?" {
		t.Errorf("cleanQuestion = %q", got)
	}
}

// 替换：input_image → input_text 证据；同图第二处指向第一处。
func TestSubstitute(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		}},
		map[string]any{"type": "function_call_output", "call_id": "c",
			"output": "data:image/png;base64,AAAA more"},
	}
	evidence := map[string]Evidence{
		ImageKey("data:image/png;base64,AAAA"): {Engine: "GLM Vision", Transcript: "## Summary\nA red error dialog."},
	}
	out := Substitute(input, evidence, nil)
	first := out[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if first["type"] != "input_text" || !strings.Contains(first["text"].(string), "A red error dialog.") {
		t.Errorf("image part must be replaced with evidence: %v", first)
	}
	if !strings.Contains(first["text"].(string), "[image read by GLM Vision]") {
		t.Errorf("evidence header must name the engine: %v", first["text"])
	}
	second := out[1].(map[string]any)["output"].(string)
	if !strings.Contains(second, "[same image as earlier in this turn") {
		t.Errorf("second occurrence must point at the record: %q", second)
	}
}

// 失败降级：stated failure 文本替换，不失败回合。
func TestSubstituteFailure(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		}},
	}
	failures := map[string]string{
		ImageKey("data:image/png;base64,AAAA"): FailureText("GLM Vision", fmt.Errorf("HTTP 503")),
	}
	out := Substitute(input, nil, failures)
	part := out[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !strings.Contains(part["text"].(string), "could not be read") {
		t.Errorf("failure must be stated: %v", part)
	}
}

// 每次读图都是独立的单次调用；Reader 层不复用缓存或 in-flight 结果
// （会话级缓存住在 server 的 SessionCache，不在这层）。
func TestReadDoesNotCacheOrShareInflight(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	reader := NewReader(func(_ context.Context, _ Engine, _, _, _ string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return "## Summary\nshared read.", nil
	})
	engines := []Engine{{Slug: "e1", DisplayName: "E1", ImageCapable: true}}
	image := ImagePart{DataURL: "data:image/png;base64,CCCC", Question: "q"}

	// 并发两次 + 串行一次：每一次都单独请求引擎。
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reader.Read(context.Background(), engines, image); err != nil {
				t.Errorf("read failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, err := reader.Read(context.Background(), engines, image); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("engine calls = %d, want 3 (no cache or inflight sharing)", calls)
	}
}

// 瞬时失败也不在 Router 内重试。
func TestReadDoesNotRetryTransientFailure(t *testing.T) {
	attempts := 0
	reader := NewReader(func(ctx context.Context, engine Engine, effort, question, dataURL string) (string, error) {
		attempts++
		return "", StatusError(429, nil)
	})
	transcript, err := reader.readOne(context.Background(),
		Engine{Slug: "e", DisplayName: "E"}, ImagePart{DataURL: "x"})
	if err == nil || transcript != "" {
		t.Errorf("429 must be returned without retry: %v %q", err, transcript)
	}
	if attempts != 1 {
		t.Errorf("429 must not be retried, attempts = %d", attempts)
	}
}

// BodySnippet：error.message 形态优先、字符串 error 次之、裸文本兜底，
// 片段限长 —— 400 的拒绝理由必须能进日志。
func TestBodySnippet(t *testing.T) {
	if got := BodySnippet([]byte(`{"error":{"message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API."}}`)); !strings.Contains(got, "must be passed back") {
		t.Errorf("error.message should win, got %q", got)
	}
	if got := BodySnippet([]byte(`{"error":"model is offline"}`)); got != "model is offline" {
		t.Errorf("string error should be extracted, got %q", got)
	}
	if got := BodySnippet([]byte("  plain   text\nbody  ")); got != "plain text body" {
		t.Errorf("whitespace should be squeezed, got %q", got)
	}
	if got := BodySnippet([]byte(`{}`)); got != "{}" {
		t.Errorf("empty error object falls back to raw, got %q", got)
	}
	if got := BodySnippet(nil); got != "" {
		t.Errorf("empty body should be empty, got %q", got)
	}
}

// 首选引擎失败时不隐式切换到备用 provider。
func TestReadStopsAfterSelectedEngineFailure(t *testing.T) {
	engines := []Engine{
		{Slug: "e1", DisplayName: "Engine One"},
		{Slug: "e2", DisplayName: "Engine Two"},
	}
	calls := 0
	reader := NewReader(func(_ context.Context, engine Engine, _, _, _ string) (string, error) {
		calls++
		if engine.Slug == "e1" {
			return "", fmt.Errorf("HTTP 400: anthropic rejected")
		}
		return "transcript", nil
	})
	_, err := reader.Read(context.Background(), engines, ImagePart{DataURL: "data:image/png;base64,QQ"})
	if err == nil || !strings.Contains(err.Error(), "Engine One") || !strings.Contains(err.Error(), "anthropic rejected") {
		t.Fatalf("selected engine failure must be returned: %v", err)
	}
	if calls != 1 {
		t.Errorf("engine calls = %d, want 1 (no fallback)", calls)
	}
}

// 证据头部必须声明"转录即全部视觉信息"并禁止尝试查看原图：
// 下游模型看到 file: 路径会发起工具轮去开原图（思考型模型
// 一轮 60-90s，2026-08-16 实测 glm 连续两轮"我先直接查看原图"）。
func TestRenderEvidenceDiscouragesViewingOriginal(t *testing.T) {
	text := RenderEvidence(Evidence{Engine: "Qwen3.5 Plus", Transcript: "## Summary\nA dialog."}, "/tmp/clip.png")
	if !strings.Contains(text, "complete visual information") {
		t.Errorf("header must state the transcript is complete:\n%s", text)
	}
	if !strings.Contains(text, "Do not attempt to open, view, or re-read") {
		t.Errorf("header must discourage viewing the original:\n%s", text)
	}
	// 抑制行在 file: 行之后、转录正文之前。
	fileIdx := strings.Index(text, "file: /tmp/clip.png")
	noteIdx := strings.Index(text, "Do not attempt")
	bodyIdx := strings.Index(text, "## Summary")
	if !(fileIdx < noteIdx && noteIdx < bodyIdx) {
		t.Errorf("header ordering wrong (file=%d note=%d body=%d):\n%s", fileIdx, noteIdx, bodyIdx, text)
	}
}
