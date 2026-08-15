package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 尺寸轮转：越过阈值归档单代，新文件从空开始继续追加。
func TestRecordRotatesOnSize(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir)
	// 手工预置一个超过阈值的文件（阈值缩到 1KB 便于测试）。
	old := usageRotateBytes
	usageRotateBytes = 1024
	defer func() { usageRotateBytes = old }()
	if err := os.WriteFile(filepath.Join(dir, "usage-events.jsonl"), []byte(strings.Repeat("x", 1200)), 0o600); err != nil {
		t.Fatal(err)
	}
	rec.Record(Event{Model: "m", Status: 200})
	if _, err := os.Stat(filepath.Join(dir, "usage-events.jsonl.1")); err != nil {
		t.Fatalf("rotated archive must exist: %v", err)
	}
	raw, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 2 || string(raw[len(raw)-1:]) != "\n" {
		t.Errorf("new file must contain the fresh record, got %d bytes", len(raw))
	}
}
