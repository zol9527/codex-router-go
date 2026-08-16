package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/loyd/codex-router/internal/spill"
)

// 未设置时：默认开 + 默认阈值。
func TestSpillDefaults(t *testing.T) {
	dir := t.TempDir()
	enabled, maxBytes, stats := ReadToolResultSpill(dir)
	if !enabled {
		t.Error("spill must default to enabled")
	}
	if maxBytes != spill.DefaultMaxBytes {
		t.Errorf("default maxBytes = %d, want %d", maxBytes, spill.DefaultMaxBytes)
	}
	if stats != (SpillStats{}) {
		t.Errorf("fresh stats must be zero, got %+v", stats)
	}
}

// 开关切换持久化，统计与阈值保留；坏文件回落默认。
func TestSpillTogglePersists(t *testing.T) {
	dir := t.TempDir()
	if err := RecordSpillStats(dir, SpillStats{ResultsSpilled: 2, BytesSaved: 100, EstimatedTokensSaved: 25}); err != nil {
		t.Fatal(err)
	}
	if err := SetToolResultSpillEnabled(dir, false); err != nil {
		t.Fatal(err)
	}
	enabled, maxBytes, stats := ReadToolResultSpill(dir)
	if enabled {
		t.Error("spill should be off after toggle")
	}
	if stats.Requests != 1 || stats.ResultsSpilled != 2 || stats.BytesSaved != 100 {
		t.Errorf("stats must survive toggle, got %+v", stats)
	}
	if maxBytes != spill.DefaultMaxBytes {
		t.Errorf("maxBytes must survive toggle, got %d", maxBytes)
	}
	if err := SetToolResultSpillEnabled(dir, true); err != nil {
		t.Fatal(err)
	}
	if enabled, _, _ := ReadToolResultSpill(dir); !enabled {
		t.Error("spill should be back on")
	}

	// 坏文件不改变默认行为。
	if err := os.WriteFile(filepath.Join(dir, "tool-result-spill.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if enabled, maxBytes, _ := ReadToolResultSpill(dir); !enabled || maxBytes != spill.DefaultMaxBytes {
		t.Error("corrupt state file must fall back to defaults")
	}
}

// max-bytes 可调，有 1024 下限；关闭状态下调整阈值不点亮开关。
func TestSpillMaxBytes(t *testing.T) {
	dir := t.TempDir()
	if err := SetToolResultSpillMaxBytes(dir, 65536); err != nil {
		t.Fatal(err)
	}
	if _, maxBytes, _ := ReadToolResultSpill(dir); maxBytes != 65536 {
		t.Errorf("maxBytes = %d, want 65536", maxBytes)
	}
	if err := SetToolResultSpillMaxBytes(dir, 512); err == nil {
		t.Error("max-bytes below 1024 must be rejected")
	}
	if err := SetToolResultSpillEnabled(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := SetToolResultSpillMaxBytes(dir, 8192); err != nil {
		t.Fatal(err)
	}
	if enabled, maxBytes, _ := ReadToolResultSpill(dir); enabled || maxBytes != 8192 {
		t.Errorf("threshold change must not flip a disabled switch, got enabled=%v maxBytes=%d", enabled, maxBytes)
	}
}

// 统计跨回合累加。
func TestSpillStatsAccumulate(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		if err := RecordSpillStats(dir, SpillStats{ResultsSpilled: 1, BytesSaved: 50, EstimatedTokensSaved: 12}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, stats := ReadToolResultSpill(dir)
	if stats.Requests != 2 || stats.ResultsSpilled != 2 || stats.BytesSaved != 100 || stats.EstimatedTokensSaved != 24 {
		t.Errorf("stats must accumulate, got %+v", stats)
	}
}

// control --json 的 toolResultSpill 块形状（Swift 解码器钉住）。
func TestSpillSnapshotShape(t *testing.T) {
	dir := t.TempDir()
	snap := ToolResultSpillSnapshot(dir)
	if snap["enabled"] != true {
		t.Error("snapshot enabled must default to true")
	}
	if snap["maxBytes"] != spill.DefaultMaxBytes {
		t.Errorf("snapshot maxBytes = %v, want %d", snap["maxBytes"], spill.DefaultMaxBytes)
	}
	stats, ok := snap["stats"].(map[string]any)
	if !ok {
		t.Fatal("snapshot stats block missing")
	}
	for _, key := range []string{"requests", "resultsSpilled", "bytesSaved", "estimatedTokensSaved"} {
		if _, ok := stats[key]; !ok {
			t.Errorf("snapshot stats missing %q", key)
		}
	}
}
