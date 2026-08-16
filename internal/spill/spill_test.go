package spill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bigText(prefix string, bytes int) string {
	return prefix + strings.Repeat("x", bytes)
}

// 低于阈值的结果逐字节保留，不产生落盘文件。
func TestProcessBelowThresholdUntouched(t *testing.T) {
	dir := t.TempDir()
	input := []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "small"},
	}
	out, stats, err := Process(input, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ToolResultsSpilled != 0 {
		t.Errorf("nothing should spill, got %+v", stats)
	}
	item := out[1].(map[string]any)
	if item["output"] != "small" {
		t.Errorf("small result must stay byte-for-byte, got %v", item["output"])
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("no spill files expected, got %d", len(entries))
	}
}

// 同一内容两次 Process 产出逐字节相同的回执（前缀缓存不翻转的根基），
// 落盘文件内容等于原文。
func TestProcessDeterministicAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	big := bigText("head-marker\n", DefaultMaxBytes+512)
	mkInput := func() []any {
		return []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": big},
			map[string]any{"type": "message", "role": "assistant", "content": "done"},
		}
	}
	first, stats1, err := Process(mkInput(), Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if stats1.ToolResultsSpilled != 1 || stats1.ToolResultBytesSaved <= 0 {
		t.Fatalf("expected one spill with savings, got %+v", stats1)
	}
	second, stats2, err := Process(mkInput(), Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Error("two Process calls on identical input must be byte-identical")
	}
	if stats1 != stats2 {
		t.Errorf("stats must match across calls: %+v vs %+v", stats1, stats2)
	}

	receipt := first[1].(map[string]any)["output"].(string)
	for _, want := range []string{
		"truncated by Model Router on first transit",
		"Full output saved to:",
		"sha256:",
		"--- beginning of original result ---",
		"head-marker",
		"--- end of original result ---",
	} {
		if !strings.Contains(receipt, want) {
			t.Errorf("receipt missing %q", want)
		}
	}
	// 回执里的路径真实存在且内容等于原文。
	idx := strings.Index(receipt, "Full output saved to: ")
	path := receipt[idx+len("Full output saved to: "):]
	path = path[:strings.IndexByte(path, '\n')]
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill file must exist at receipt path: %v", err)
	}
	if string(raw) != big {
		t.Error("spill file content must equal the original output")
	}
	if !strings.HasPrefix(filepath.Base(path), "spill-") {
		t.Errorf("spill file name must be content-addressed, got %q", filepath.Base(path))
	}
	// 内容寻址幂等：同内容只落一个文件。
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected exactly one spill file, got %d", len(entries))
	}
}

// 落盘文件被清理后，下一次 Process 凭原文自愈重建，回执字节不变。
func TestProcessHealsDeletedFile(t *testing.T) {
	dir := t.TempDir()
	big := bigText("h", DefaultMaxBytes+64)
	input := []any{
		map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "exec"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": big},
	}
	first, _, err := Process(input, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.Remove(filepath.Join(dir, e.Name()))
	}
	healed, _, err := Process(input, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(healed)
	if string(a) != string(b) {
		t.Error("receipt must stay identical after spill file deletion (self-heal)")
	}
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	}
}

// 图像/混合内容与 []any 文本形态：非纯文本不截；文本 parts 超限照截。
func TestProcessNonTextualUntouched(t *testing.T) {
	dir := t.TempDir()
	input := []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "view_image", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": []any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		}},
		map[string]any{"type": "function_call", "call_id": "c2", "name": "shell", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c2", "output": []any{
			map[string]any{"type": "input_text", "text": bigText("t", DefaultMaxBytes+128)},
		}},
	}
	out, stats, err := Process(input, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ToolResultsSpilled != 1 {
		t.Fatalf("only the textual part-array result should spill, got %+v", stats)
	}
	if _, ok := out[1].(map[string]any)["output"].([]any); !ok {
		t.Error("image result must keep its structured output")
	}
	if s, _ := out[3].(map[string]any)["output"].(string); !strings.Contains(s, "truncated by Model Router") {
		t.Error("textual part-array result should be replaced with a receipt")
	}
}

// 阈值可调：MaxBytes 覆盖默认值。
func TestProcessMaxBytesOption(t *testing.T) {
	dir := t.TempDir()
	text := bigText("m", 2048)
	input := []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": text},
	}
	out, stats, _ := Process(input, Options{Dir: dir, MaxBytes: 1024})
	if stats.ToolResultsSpilled != 1 {
		t.Fatalf("2KB output should spill at 1KB threshold, got %+v", stats)
	}
	out, stats, _ = Process(input, Options{Dir: dir, MaxBytes: 4096})
	if stats.ToolResultsSpilled != 0 {
		t.Fatalf("2KB output should not spill at 4KB threshold, got %+v", stats)
	}
	if out[1].(map[string]any)["output"] != text {
		t.Error("output must stay original below threshold")
	}
}

// 预览重叠守卫：多字节密集内容在低阈值下字节数先过阈、rune 数还
// 没超过两倍预览 —— 头尾拼起来覆盖全文，此时保留原文（截断只会
// 重复内容并把 omitted 记成负数）。
func TestProcessPreviewOverlapKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	// 1500 个三字节汉字 = 4500 字节 ≥ 阈值 4096，但 rune 数 1500 <
	// 2×1024，预览必然重叠。
	text := strings.Repeat("汉", 1500)
	input := []any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": text},
	}
	out, stats, err := Process(input, Options{Dir: dir, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ToolResultsSpilled != 0 {
		t.Errorf("overlap case must not spill, got %+v", stats)
	}
	if item, _ := out[1].(map[string]any); item["output"] != text {
		t.Error("overlap case must keep the original output byte-for-byte")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("no spill files expected, got %d", len(entries))
	}

	// 对照：同样内容换成 ASCII（每 rune 1 字节），阈值不变时 rune 数
	// 4500 > 2×1024，中段可省 —— 照常截断，证明守卫只拦重叠边界。
	ascii := strings.Repeat("a", 4500)
	input[1] = map[string]any{"type": "function_call_output", "call_id": "c1", "output": ascii}
	_, stats, err = Process(input, Options{Dir: dir, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ToolResultsSpilled != 1 {
		t.Errorf("ASCII counterpart should spill normally, got %+v", stats)
	}
}

// Cleanup 只删保留期外的 spill 文件；其他名字/新文件不动。
func TestCleanupRemovesExpiredOnly(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, filePrefix+"old.txt")
	fresh := filepath.Join(dir, filePrefix+"fresh.txt")
	other := filepath.Join(dir, "notes.txt")
	// 崩溃孤儿的临时文件同样按保留期回收；新鲜的临时文件（写入
	// 进行中）不动。
	tmpOld := filepath.Join(dir, tmpPrefix+"crashed")
	tmpFresh := filepath.Join(dir, tmpPrefix+"inflight")
	for _, p := range []string{old, fresh, other, tmpOld, tmpFresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expired := time.Now().Add(-(Retention + time.Hour))
	if err := os.Chtimes(old, expired, expired); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmpOld, expired, expired); err != nil {
		t.Fatal(err)
	}
	removed, err := Cleanup(dir, Retention)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("expected 2 removals (spill + tmp), got %d", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expired spill file must be removed")
	}
	if _, err := os.Stat(tmpOld); !os.IsNotExist(err) {
		t.Error("expired tmp file must be removed")
	}
	for _, p := range []string{fresh, other, tmpFresh} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive cleanup", p)
		}
	}
	if removed, _ := Cleanup(filepath.Join(dir, "missing"), Retention); removed != 0 {
		t.Error("missing dir is a no-op")
	}
}
