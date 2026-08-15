package main

// vision-bridge control 动作与快照块的形状测试：这些是 tray 设置页
// 视觉卡的调用面 —— Swift 侧按钮一次失败就是一次"按钮不成功"，
// 每个 action 与 visionBridge 的键在这里钉住。

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
	"github.com/loyd/codex-router/internal/vision"
)

// captureStdout 捕获被测函数写到 stdout 的内容（printVisionStatus 用）。
// 读取放 goroutine —— 输出超过管道缓冲（~64KB）时被测函数才不会阻塞。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(reader)
		done <- string(out)
	}()
	fn()
	writer.Close()
	os.Stdout = original
	return <-done
}

func visionTestRegistry() *registry.Registry {
	return &registry.Registry{
		Providers: map[string]*registry.Provider{
			"test-provider": {ID: "test-provider", DisplayName: "Test", Kind: "openai-compatible"},
		},
		Models: []*registry.Model{
			{Slug: "test-provider/vision-model", Provider: "test-provider", Listed: true,
				DisplayName: "Vision Model", InputModalities: []string{"text", "image"}},
			{Slug: "test-provider/text-model", Provider: "test-provider", Listed: true,
				DisplayName: "Text Model", InputModalities: []string{"text"}},
		},
	}
}

func writeTestCatalog(t *testing.T, dir string) {
	t.Helper()
	catalog := `{"models":[
		{"slug":"gpt-5.6-luna","display_name":"Luna","visibility":"list",
		 "input_modalities":["text","image"],"priority":10,
		 "supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],
		 "default_reasoning_level":"high"}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "merged-models.json"), []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 开关往返：tray 的 on/off 按钮路径。
func TestControlVisionBridgeOnOff(t *testing.T) {
	dir := t.TempDir()
	for _, action := range []string{"on", "off", "on"} {
		if err := controlVisionBridge(dir, visionTestRegistry(), []string{action}); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	settings, configured := vision.ReadSettings(dir)
	if !configured {
		t.Fatal("toggle must leave a configured settings file")
	}
	if !settings.EffectiveEnabled(configured) {
		t.Fatalf("final state must be on, got %+v", settings)
	}
}

// 引擎 pin：auto/local/合法 slug 通过，未知 slug 必须报错
// （pin 失效的引擎会让每次贴图静默降级）。
func TestControlVisionBridgeEngine(t *testing.T) {
	dir := t.TempDir()
	writeTestCatalog(t, dir)
	reg := visionTestRegistry()

	if err := controlVisionBridge(dir, reg, []string{"engine", "auto"}); err != nil {
		t.Fatalf("auto: %v", err)
	}
	settings, _ := vision.ReadSettings(dir)
	if settings.Engine != "" {
		t.Errorf("auto must clear the pin, got %q", settings.Engine)
	}

	if err := controlVisionBridge(dir, reg, []string{"engine", "gpt-5.6-luna", "high"}); err != nil {
		t.Fatalf("native slug: %v", err)
	}
	settings, _ = vision.ReadSettings(dir)
	if settings.Engine != "gpt-5.6-luna" || settings.Effort != "high" {
		t.Errorf("engine+effort must land together: %+v", settings)
	}

	// registry 视觉模型同样可 pin；随后 effort 归还默认。
	if err := controlVisionBridge(dir, reg, []string{"engine", "test-provider/vision-model"}); err != nil {
		t.Fatalf("registry slug: %v", err)
	}
	if err := controlVisionBridge(dir, reg, []string{"effort", "default"}); err != nil {
		t.Fatalf("effort default: %v", err)
	}
	settings, _ = vision.ReadSettings(dir)
	if settings.Effort != "" {
		t.Errorf("default must clear effort, got %q", settings.Effort)
	}

	if err := controlVisionBridge(dir, reg, []string{"engine", "no-such-engine"}); err == nil {
		t.Error("unknown engine slug must fail visibly")
	}
	// 纯文本模型不是引擎。
	if err := controlVisionBridge(dir, reg, []string{"engine", "test-provider/text-model"}); err == nil {
		t.Error("text-only model must not be pinnable as engine")
	}
}

// 本地引擎 pin：tray 的 useLocalVisionModel 路径。
func TestControlVisionBridgeLocal(t *testing.T) {
	dir := t.TempDir()
	if err := controlVisionBridge(dir, visionTestRegistry(), []string{"local", "qwen2.5vl:3b"}); err != nil {
		t.Fatalf("local: %v", err)
	}
	settings, _ := vision.ReadSettings(dir)
	if settings.Engine != vision.LocalEngineSlug || settings.LocalModel != "qwen2.5vl:3b" {
		t.Fatalf("local pin must land: %+v", settings)
	}
}

// 快照块形状：enabled 恒在；engine/effort 只有 pin 时出现；
// nativeEngines 来自 merged 目录；resolvedEngine 在 auto 且有候选时非空。
func TestVisionBridgeSnapshotShape(t *testing.T) {
	dir := t.TempDir()
	writeTestCatalog(t, dir)
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg := visionTestRegistry()

	snapshot := visionBridgeSnapshot(st, reg)
	if snapshot["enabled"] != true {
		t.Errorf("never-configured bridge must snapshot as enabled (default on), got %v", snapshot["enabled"])
	}
	if _, ok := snapshot["engine"]; ok {
		t.Error("engine must be absent when nothing is pinned")
	}
	natives, _ := snapshot["nativeEngines"].([]map[string]any)
	if len(natives) != 1 || natives[0]["slug"] != "gpt-5.6-luna" {
		t.Errorf("nativeEngines must come from the merged catalog: %v", snapshot["nativeEngines"])
	}
	if snapshot["resolvedEngine"] != "gpt-5.6-luna" {
		t.Errorf("auto resolution must pick the ranked native engine, got %v", snapshot["resolvedEngine"])
	}

	// pin local 后：local 块出现、resolvedEngine 变为 local。
	if err := controlVisionBridge(dir, reg, []string{"local", "qwen2.5vl:3b"}); err != nil {
		t.Fatal(err)
	}
	if err := controlVisionBridge(dir, reg, []string{"effort", "low"}); err != nil {
		t.Fatal(err)
	}
	snapshot = visionBridgeSnapshot(st, reg)
	if snapshot["engine"] != "local" || snapshot["effort"] != "low" {
		t.Errorf("pins must surface in the snapshot: %v", snapshot)
	}
	local, _ := snapshot["local"].(map[string]any)
	if local["model"] != "qwen2.5vl:3b" {
		t.Errorf("local pin model must surface: %v", snapshot["local"])
	}
	if snapshot["resolvedEngine"] != "local" {
		t.Errorf("local pin must resolve to local, got %v", snapshot["resolvedEngine"])
	}

	// off 之后 enabled=false —— tray 的开关回读路径。
	if err := controlVisionBridge(dir, reg, []string{"off"}); err != nil {
		t.Fatal(err)
	}
	snapshot = visionBridgeSnapshot(st, reg)
	if snapshot["enabled"] != false {
		t.Errorf("off must snapshot enabled=false, got %v", snapshot["enabled"])
	}
}

// status 动作输出的就是快照块（写操作后 tray 刷新看到同一形状）。
func TestControlVisionBridgeStatusOutput(t *testing.T) {
	dir := t.TempDir()
	writeTestCatalog(t, dir)
	stdout := captureStdout(t, func() {
		if err := controlVisionBridge(dir, visionTestRegistry(), []string{"status"}); err != nil {
			t.Fatal(err)
		}
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &parsed); err != nil {
		t.Fatalf("status must print the snapshot JSON block: %v (%q)", err, stdout)
	}
	if parsed["enabled"] != true {
		t.Errorf("status enabled mismatch: %v", parsed["enabled"])
	}
}
