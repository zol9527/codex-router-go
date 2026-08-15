package state

import (
	"os"
	"path/filepath"
	"testing"
)

// aging 开关红线：默认开（Go 版自移植起常开，引入开关不得改变行为）；
// 关掉要持久；统计只累计、开关切换不丢统计。
func TestToolResultAgingDefaultsOn(t *testing.T) {
	enabled, stats := ReadToolResultAging(t.TempDir())
	if !enabled || stats.Requests != 0 {
		t.Fatalf("defaults = enabled:%v stats:%+v, want on/zero", enabled, stats)
	}
}

func TestToolResultAgingToggleAndStats(t *testing.T) {
	dir := t.TempDir()
	if err := SetToolResultAgingEnabled(dir, false); err != nil {
		t.Fatal(err)
	}
	enabled, stats := ReadToolResultAging(dir)
	if enabled || stats.Requests != 0 {
		t.Fatalf("after off = enabled:%v", enabled)
	}
	if err := RecordAgingStats(dir, AgingStats{ResultsAged: 3, BytesSaved: 900, EstimatedTokensSaved: 225}); err != nil {
		t.Fatal(err)
	}
	if err := RecordAgingStats(dir, AgingStats{ResultsAged: 1, BytesSaved: 100, EstimatedTokensSaved: 25}); err != nil {
		t.Fatal(err)
	}
	// 关着也不妨碍统计累计（历史记录与开关是两回事）。
	enabled, stats = ReadToolResultAging(dir)
	if enabled {
		t.Fatal("stats recording must not flip the toggle back on")
	}
	if stats.Requests != 2 || stats.ResultsAged != 4 || stats.BytesSaved != 1000 {
		t.Fatalf("accumulated = %+v", stats)
	}
	if err := SetToolResultAgingEnabled(dir, true); err != nil {
		t.Fatal(err)
	}
	_, stats = ReadToolResultAging(dir)
	if stats.Requests != 2 {
		t.Fatal("toggling must preserve stats")
	}
	info, err := os.Stat(filepath.Join(dir, "tool-result-aging.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file perms: %v %v", err, info)
	}
}
