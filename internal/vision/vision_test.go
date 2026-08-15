package vision

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// 引擎解析：pin 优先、pin 失效（显式）返回空、auto 跳过 loopback、
// 回退列表最多 3 个、本地 pin 不回退。
func TestResolveEngines(t *testing.T) {
	candidates := []Engine{
		{Slug: "zai/glm-v", DisplayName: "GLM Vision", GatewayModel: "glm-v", Priority: 10, ImageCapable: true},
		{Slug: "local-ollama", DisplayName: "Local", Local: true, Priority: 1, ImageCapable: true},
		{Slug: "oc/gpt-v", DisplayName: "GPT Vision", GatewayModel: "gpt-v", Priority: 20, ImageCapable: true},
		{Slug: "oc/claude-v", DisplayName: "Claude Vision", GatewayModel: "claude-v", Priority: 30, ImageCapable: true},
	}
	enabled := true
	on := Settings{Enabled: &enabled}

	// auto：跳过 loopback 的 local-ollama，取 GLM Vision + 备用 2 个。
	engines := ResolveEngines(candidates, on, false)
	if len(engines) != 3 || engines[0].Slug != "zai/glm-v" {
		t.Fatalf("auto resolution wrong: %+v", engines)
	}
	for _, engine := range engines {
		if engine.Loopback() {
			t.Errorf("auto must never nominate loopback: %v", engine.Slug)
		}
	}
	// 显式 pin 失效 → 空（操作者可见）。
	engines = ResolveEngines(candidates, Settings{Enabled: &enabled, Engine: "gone"}, true)
	if len(engines) != 0 {
		t.Errorf("explicit dead pin must resolve nothing, got %+v", engines)
	}
	// 默认引擎失效 → 静默落到排名首位。
	engines = ResolveEngines(candidates, Settings{Enabled: &enabled, Engine: "gone", Defaulted: true}, true)
	if len(engines) == 0 || engines[0].Slug != "zai/glm-v" {
		t.Errorf("dead default falls to ranked head: %+v", engines)
	}
	// 本地 pin：单引擎，绝不回退到 provider。
	engines = ResolveEngines(candidates, Settings{Enabled: &enabled, Engine: LocalEngineSlug}, true)
	if len(engines) != 1 || !engines[0].Local {
		t.Fatalf("local pin must be solo: %+v", engines)
	}
	// 关闭 → 空。
	off := Settings{Enabled: &enabled}
	off.Enabled = &[]bool{false}[0]
	if engines = ResolveEngines(candidates, off, true); len(engines) != 0 {
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

// 一图一购：同图并发读只打一次引擎；第二次读走缓存。
func TestReadSharedInflight(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Write([]byte(`{"choices":[{"message":{"content":"## Summary\nshared read."}}]}`))
	}))
	defer upstream.Close()

	reader := NewReader(func(ctx context.Context, engine Engine, effort, question, dataURL string) (string, error) {
		status, body, err := PostJSON(ctx, http.DefaultClient, upstream.URL, nil,
			ChatDescribeRequest("m", question, dataURL))
		if err != nil {
			return "", err
		}
		if status != 200 {
			return "", StatusError(status, nil)
		}
		return ParseChatDescribeResponse(body)
	})
	engines := []Engine{{Slug: "e1", DisplayName: "E1", ImageCapable: true}}
	image := ImagePart{DataURL: "data:image/png;base64,CCCC", Question: "q"}

	// 并发两次 + 串行一次：引擎只被调一次。
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
	if calls != 1 {
		t.Errorf("engine calls = %d, want 1 (inflight share + cache)", calls)
	}
}

// 回退：首引擎 503 后备用引擎接手，证据标记 fellBack。
func TestReadFallback(t *testing.T) {
	var callCount int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// 首引擎 3 次尝试全 503（重试也耗尽）。
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"from fallback"}}]}`))
	}))
	defer upstream.Close()

	reader := NewReader(func(ctx context.Context, engine Engine, effort, question, dataURL string) (string, error) {
		status, body, err := PostJSON(ctx, http.DefaultClient, upstream.URL, nil, nil)
		_ = body
		if err != nil {
			return "", err
		}
		if status != 200 {
			return "", StatusError(status, nil)
		}
		return "from " + engine.Slug, nil
	})
	_ = reader
	// 直接测 readWithFallback（绕开 HTTP 层）。
	reader2 := NewReader(func(ctx context.Context, engine Engine, effort, question, dataURL string) (string, error) {
		if engine.Slug == "primary" {
			return "", StatusError(503, nil)
		}
		return "transcript from " + engine.Slug, nil
	})
	engines := []Engine{
		{Slug: "primary", DisplayName: "Primary", ImageCapable: true},
		{Slug: "backup", DisplayName: "Backup", ImageCapable: true},
	}
	evidence, err := reader2.Read(context.Background(), engines, ImagePart{DataURL: "data:image/png;base64,X"})
	if err != nil {
		t.Fatalf("fallback must succeed: %v", err)
	}
	if evidence.Engine != "Backup" || !evidence.FellBack {
		t.Errorf("evidence must name fallback engine: %+v", evidence)
	}
	if !strings.Contains(evidence.Transcript, "backup") {
		t.Errorf("transcript from backup: %q", evidence.Transcript)
	}
}

// 瞬时失败重试：429 重试后成功；400 不重试。
func TestTransientRetry(t *testing.T) {
	attempts := 0
	reader := NewReader(func(ctx context.Context, engine Engine, effort, question, dataURL string) (string, error) {
		attempts++
		if attempts == 1 {
			return "", StatusError(429, nil)
		}
		return "recovered", nil
	})
	transcript, err := reader.readOneWithRetry(context.Background(),
		Engine{Slug: "e", DisplayName: "E"}, ImagePart{DataURL: "x"})
	if err != nil || transcript != "recovered" {
		t.Errorf("429 must be retried: %v %q", err, transcript)
	}

	attempts = 0
	reader2 := NewReader(func(ctx context.Context, engine Engine, effort, question, dataURL string) (string, error) {
		attempts++
		return "", StatusError(400, nil)
	})
	if _, err := reader2.readOneWithRetry(context.Background(), Engine{Slug: "e"}, ImagePart{}); err == nil {
		t.Error("400 must not retry-succeed")
	}
	if attempts != 1 {
		t.Errorf("400 must not be retried, attempts = %d", attempts)
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

// anthropic 代读：data URL 拆解、请求形状（system + image source 块）、
// 响应解析（text block 提取）。
func TestAnthropicDescribe(t *testing.T) {
	media, data, ok := splitDataURL("data:image/png;base64,AAAB")
	if !ok || media != "image/png" || data != "AAAB" {
		t.Fatalf("splitDataURL wrong: %q %q %v", media, data, ok)
	}
	if _, _, ok := splitDataURL("https://example.com/x.png"); ok {
		t.Error("non-data URL must be rejected")
	}
	if _, _, ok := splitDataURL("data:image/png,AAAB"); ok {
		t.Error("non-base64 data URL must be rejected")
	}

	body, ok := AnthropicDescribeRequest("qwen3.7-max", "read the error", "data:image/png;base64,AAAB")
	if !ok {
		t.Fatal("request must build")
	}
	if body["model"] != "qwen3.7-max" || body["max_tokens"] != 4096 {
		t.Errorf("model/max_tokens wrong: %v %v", body["model"], body["max_tokens"])
	}
	if sys, _ := body["system"].(string); !strings.Contains(sys, "read the error") {
		t.Errorf("question must fold into system instructions: %q", sys)
	}
	blocks := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	image := blocks[1].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("second block must be image, got %v", image["type"])
	}
	source := image["source"].(map[string]any)
	if source["media_type"] != "image/png" || source["data"] != "AAAB" || source["type"] != "base64" {
		t.Errorf("image source wrong: %v", source)
	}

	got, err := ParseAnthropicDescribeResponse([]byte(`{"content":[{"type":"text","text":"A dialog."},{"type":"thinking","text":"..."},{"type":"text","text":"Reads boom."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "A dialog.\nReads boom." {
		t.Errorf("text blocks wrong: %q", got)
	}
	if _, err := ParseAnthropicDescribeResponse([]byte(`{"content":[]}`)); err == nil {
		t.Error("empty content must error")
	}
}

// 回退链的失败必须随证据上浮：成功兜底时首选引擎的错误进入
// PriorFailures（否则排障只能看到 "fellBack=true" 而不知为何）。
func TestReadWithFallbackSurfacesPriorFailures(t *testing.T) {
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
	evidence, err := reader.Read(context.Background(), engines, ImagePart{DataURL: "data:image/png;base64,QQ"})
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.FellBack || evidence.Engine != "Engine Two" {
		t.Fatalf("fallback outcome wrong: %+v", evidence)
	}
	if len(evidence.PriorFailures) != 1 || !strings.Contains(evidence.PriorFailures[0], "Engine One") || !strings.Contains(evidence.PriorFailures[0], "anthropic rejected") {
		t.Errorf("prior failures must surface: %v", evidence.PriorFailures)
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
