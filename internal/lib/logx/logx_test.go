package logx

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupErrFile 模式的基础断言全集：JSON 行可解析、级别过滤生效、
// kv 落盘正确。每个用例独立 Setup + 还原。
func TestFileModeJSONAndLevels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.log")
	restore := Swap(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	defer restore()

	if err := Setup(Config{Level: "warn", File: path}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		Close()
		// 还原为 stderr text，避免污染同包其他用例。
		Swap(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	}()

	Info("should be filtered")
	Warn("degraded", "model", "glm-5.3", "suppressed", 3)
	Error("request failed", "status", 502)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (info filtered), got %d: %q", len(lines), raw)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not JSON: %v (%q)", err, lines[0])
	}
	if first["level"] != "WARN" || first["msg"] != "degraded" || first["model"] != "glm-5.3" {
		t.Fatalf("unexpected first line: %v", first)
	}
	if first["suppressed"].(float64) != 3 {
		t.Fatalf("kv int lost: %v", first)
	}
}

// stderr(text) 模式：Setup 空文件路径永不失败、text 格式含 level=msg=。
func TestStderrTextMode(t *testing.T) {
	var buf bytes.Buffer
	if err := Setup(Config{}); err != nil {
		t.Fatal(err)
	}
	defer func() { Swap(slog.New(slog.NewTextHandler(os.Stderr, nil))) }()

	restore := Swap(slog.New(slog.NewTextHandler(&buf, nil)))
	defer restore()
	Info("listening", "port", 4202)

	out := buf.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "msg=listening") || !strings.Contains(out, "port=4202") {
		t.Fatalf("unexpected text output: %q", out)
	}
}

// 非法级别值必须显式报错，而不是静默落回默认档。
func TestUnknownLevelRejected(t *testing.T) {
	if err := Setup(Config{Level: "verbose"}); err == nil {
		t.Fatal("want error for unknown level")
	}
}

// 文件不可写（路径落在文件上）必须报错 —— 显式指定落盘却打不开，
// 静默降级等于丢日志。
func TestUnwritableFileRejected(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(Config{File: filepath.Join(blocker, "router.log")}); err == nil {
		t.Fatal("want error for unwritable log path")
	}
}

// 轮转：写前尺寸检查、单代归档、无丢行。边界语义：检查发生在写前，
// 单个文件可短暂超过阈值至多一行（maxBytes + maxLineLen - 1）。
func TestRotationSingleGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.log")
	w, err := newRotatingWriter(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	const lineLen = 21
	for i := 0; i < 8; i++ {
		if _, err := w.Write([]byte(strings.Repeat("x", 20) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	archived, _ := os.ReadFile(path + ".1")
	current, _ := os.ReadFile(path)
	if got := len(archived) + len(current); got != 8*lineLen {
		t.Fatalf("lines lost across rotation: want %d bytes total, got %d", 8*lineLen, got)
	}
	if len(current) > 64+lineLen {
		t.Fatalf("current file overshot threshold by more than one line: %d bytes", len(current))
	}
}

// rotate 后旧句柄失效场景：Close 之后的 Write 丢弃而非 panic。
func TestWriteAfterCloseDiscards(t *testing.T) {
	dir := t.TempDir()
	w, err := newRotatingWriter(filepath.Join(dir, "router.log"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("late\n")); err != nil || n != 5 {
		t.Fatalf("write after close should discard silently, got n=%d err=%v", n, err)
	}
}

// With 派生的子 logger 预置 kv 生效（请求作用域关联键）。
func TestWithPresetKV(t *testing.T) {
	var buf bytes.Buffer
	restore := Swap(slog.New(slog.NewTextHandler(&buf, nil)))
	defer restore()

	log := With("req", "abcd1234")
	log.Info("request done", "status", 200)

	out := buf.String()
	if !strings.Contains(out, "req=abcd1234") || !strings.Contains(out, "status=200") {
		t.Fatalf("preset kv missing: %q", out)
	}
}

// NewID 输出 8 hex 且大批量下格式稳定（碰撞概率断言留给数学，这里
// 只钉格式契约 —— 下游日志以 8 字符做关联键）。
func TestNewIDFormat(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != 8 {
			t.Fatalf("id length = %d, want 8 (%q)", len(id), id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("id not hex: %q", err)
		}
		seen[id] = true
	}
	if len(seen) < 900 {
		t.Fatalf("1000 draws produced only %d unique ids", len(seen))
	}
}

// Swap 还原闭包必须完整恢复前一个 logger（测试隔离的基石）。
func TestSwapRestore(t *testing.T) {
	base := slog.New(slog.NewTextHandler(os.Stderr, nil))
	restoreBase := Swap(base)
	defer restoreBase()

	var buf bytes.Buffer
	temp := slog.New(slog.NewTextHandler(&buf, nil))
	restore := Swap(temp)
	Info("during")
	restore()

	if !strings.Contains(buf.String(), "during") {
		t.Fatal("swapped logger did not capture")
	}
	if Default() != base {
		t.Fatalf("restore did not reinstate previous logger")
	}
}

// 并发写：多 goroutine 同打无竞态、无行撕裂（go test -race 的主战场）。
// 单代覆盖语义下更早的归档代会被丢弃，因此不断言总行数守恒，只断言
// 读回的每一行都是完整 JSON。
func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.log")
	if err := Setup(Config{File: path, RotateBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		Close()
		Swap(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	}()

	const workers, each = 8, 50
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				Info("concurrent", "worker", n, "seq", j)
			}
		}(i)
	}
	wg.Wait()

	total := 0
	for _, p := range []string{path, path + ".1"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("corrupt JSON line after concurrent writes: %v (%q)", err, line)
			}
			if m["worker"] == nil || m["seq"] == nil {
				t.Fatalf("torn kv in line: %q", line)
			}
			total++
		}
	}
	if total == 0 || total > workers*each {
		t.Fatalf("implausible line count: %d", total)
	}
}
