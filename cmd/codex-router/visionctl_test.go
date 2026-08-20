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

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/state"
	"github.com/loyd/codex-router/internal/domain/vision"
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

// 档位 pin：level 落盘、"default" 归一为空（档位交还引擎默认）。
func TestControlVisionBridgeEffort(t *testing.T) {
	dir := t.TempDir()
	writeTestCatalog(t, dir)
	reg := visionTestRegistry()

	if err := controlVisionBridge(dir, reg, []string{"effort", "high"}); err != nil {
		t.Fatalf("effort: %v", err)
	}
	settings, _ := vision.ReadSettings(dir)
	if settings.Effort != "high" {
		t.Errorf("effort must land, got %+v", settings)
	}

	if err := controlVisionBridge(dir, reg, []string{"effort", "default"}); err != nil {
		t.Fatalf("effort default: %v", err)
	}
	settings, _ = vision.ReadSettings(dir)
	if settings.Effort != "" {
		t.Errorf("default must clear effort, got %q", settings.Effort)
	}

	if err := controlVisionBridge(dir, reg, []string{"engine", "gpt-5.6-luna"}); err == nil {
		t.Error("engine selection is gone; the action must fail visibly")
	}
	if err := controlVisionBridge(dir, reg, []string{"local", "qwen2.5vl:3b"}); err == nil {
		t.Error("local pin is gone; the action must fail visibly")
	}
}

// 快照块形状：enabled 恒在；resolvedEngine 来自 merged 目录的
// native 候选（引擎固定 native，没有选择字段）。
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
	if snapshot["resolvedEngine"] != "gpt-5.6-luna" {
		t.Errorf("resolution must pick the ranked native engine, got %v", snapshot["resolvedEngine"])
	}
	for _, gone := range []string{"engine", "local", "paidEngines", "nativeEngines", "download", "hostMemGib"} {
		if _, ok := snapshot[gone]; ok {
			t.Errorf("selection-era key %q must be gone from the snapshot", gone)
		}
	}

	// 档位 pin 进快照。
	if err := controlVisionBridge(dir, reg, []string{"effort", "low"}); err != nil {
		t.Fatal(err)
	}
	snapshot = visionBridgeSnapshot(st, reg)
	if snapshot["effort"] != "low" {
		t.Errorf("effort must surface in the snapshot: %v", snapshot)
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
