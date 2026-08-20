package vision

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	enabled := true
	written := Settings{
		Enabled: &enabled,
		Effort:  "high",
	}
	if err := WriteSettings(dir, written); err != nil {
		t.Fatalf("write: %v", err)
	}
	read, configured := ReadSettings(dir)
	if !configured {
		t.Fatal("settings must read back as configured after write")
	}
	if read.EffectiveEnabled(configured) != true {
		t.Error("enabled must round-trip")
	}
	if read.Effort != "high" {
		t.Errorf("fields did not round-trip: %+v", read)
	}
	info, err := os.Stat(filepath.Join(dir, "vision-bridge.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("settings file must be 0600, got %v", info.Mode().Perm())
	}
}

func TestWriteSettingsPreservesUnrelatedFields(t *testing.T) {
	dir := t.TempDir()
	// pin 档位后单独切开关：其余字段必须保留（tray 的开关与档位
	// 是两个独立入口）。
	if err := WriteSettings(dir, Settings{Effort: "low"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	off := false
	if err := WriteSettings(dir, Settings{Enabled: &off, Effort: "low"}); err != nil {
		t.Fatalf("write off: %v", err)
	}
	read, configured := ReadSettings(dir)
	if !configured || read.EffectiveEnabled(configured) {
		t.Fatalf("off must round-trip, got %+v configured=%v", read, configured)
	}
	if read.Effort != "low" {
		t.Errorf("effort must survive the toggle: %+v", read)
	}
}

func TestNativeEnginesFromCatalogFile(t *testing.T) {
	dir := t.TempDir()
	// 数组 modality（merged 目录的实际形态）与字符串形态都要认；
	// 被 exclude 的 slug（registry 条目反馈回来的克隆）必须剔除；
	// 纯文本条目不算引擎。
	catalog := `{"models":[
		{"slug":"gpt-5.6-luna","display_name":"Luna","visibility":"list",
		 "input_modalities":["text","image"],"priority":10,
		 "supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],
		 "default_reasoning_level":"high"},
		{"slug":"legacy-string-modality","display_name":"Legacy","visibility":"list",
		 "input_modalities":"text image","priority":20},
		{"slug":"zai-coding/clone","display_name":"Clone","visibility":"list",
		 "input_modalities":["text","image"],"priority":5},
		{"slug":"text-only","display_name":"Text","visibility":"list",
		 "input_modalities":["text"],"priority":1}
	]}`
	path := filepath.Join(dir, "merged-models.json")
	if err := os.WriteFile(path, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	engines := NativeEnginesFromCatalogFile(path, map[string]bool{"zai-coding/clone": true})
	slugs := map[string]Engine{}
	for _, engine := range engines {
		slugs[engine.Slug] = engine
	}
	if len(engines) != 2 {
		t.Fatalf("want luna+legacy, got %d engines", len(engines))
	}
	luna := slugs["gpt-5.6-luna"]
	if !luna.Native || !luna.ImageCapable {
		t.Errorf("luna must be a native image engine: %+v", luna)
	}
	if len(luna.Efforts) != 2 || luna.DefaultEffort != "high" {
		t.Errorf("luna effort ladder must parse: %+v", luna)
	}
	if _, ok := slugs["legacy-string-modality"]; !ok {
		t.Error("string-form modality must be accepted")
	}
	if _, ok := slugs["zai-coding/clone"]; ok {
		t.Error("excluded slug must not be an engine candidate")
	}
	if _, ok := slugs["text-only"]; ok {
		t.Error("text-only model must not be an engine candidate")
	}
}

func TestNativeEnginesFromCatalogFileMissing(t *testing.T) {
	if engines := NativeEnginesFromCatalogFile(filepath.Join(t.TempDir(), "absent.json"), nil); engines != nil {
		t.Errorf("missing catalog must yield nil, got %v", engines)
	}
}
