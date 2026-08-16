// Package spill 实现工具结果的"首过境确定性截断"（opencode v2 模式）。
//
// 与旧 tool-result aging 的本质区别：截断判定是内容的纯函数——文本
// 结果达到 MaxBytes 即截，与它在历史中的位置、会话长短无关。因此
// 同一内容在任何请求里都产出逐字节相同的回执，上游前缀缓存永不
// 翻转（aging 的 frontier 滚动会在结果"变老"的那一刻改写历史，
// 从分叉点起缓存全灭，且 turn 内工具往返同样会触发翻转）。
//
// 全文按内容寻址落盘（sha256 前 8 字节 hex 命名）：回执带绝对路径，
// 模型可用 Read(offset/limit) 或 Grep 取回中段。Codex 端永远重放
// 原始结果，所以即便落盘文件被保留期清理删掉，下一次请求也会凭
// 原文重新写出来（自愈），回执字节不变。
package spill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultMaxBytes 是截断阈值：原始输出达到该字节数即落盘+回执。
	DefaultMaxBytes = 32 * 1024
	// DefaultPreviewRunes 是回执保留的头/尾预览长度（rune）。
	DefaultPreviewRunes = 1024
	// Retention 是落盘文件的保留期，janitor 按此清理。
	Retention = 7 * 24 * time.Hour
	// DirName 是状态目录下的 spill 子目录名。
	DirName = "spill"
	// filePrefix 是落盘文件名前缀，后接内容寻址 hex。
	filePrefix = "spill-"
	// tmpPrefix 是写入中途的临时文件前缀；写完即 rename 成 filePrefix
	// 名，残留的 tmp 文件是崩溃孤儿，Cleanup 一并按保留期回收。
	tmpPrefix = ".tmp-spill-"
)

// Options 控制一次 Process 的行为；零值字段取默认。
type Options struct {
	// Dir 是落盘根目录；空串表示不处理（直接原样返回）。
	Dir string
	// MaxBytes 是截断阈值（字节）。
	MaxBytes int
	// PreviewRunes 是头/尾预览长度（rune）。
	PreviewRunes int
}

// Stats 是单次 Process 的截断统计（usage 落账用）。
type Stats struct {
	ToolResultsSpilled   int   `json:"toolResultsSpilled"`
	ToolResultBytesSaved int64 `json:"toolResultBytesSaved"`
}

// Process 对 Responses input 数组执行确定性截断：文本形态的
// function_call_output / custom_tool_call_output 且原始输出 ≥ MaxBytes
// 的，全文落盘并替换为回执；其余条目逐字节保留。返回（可能相同的）
// 数组、统计、以及首个落盘失败（失败条目保留原文，不影响其余）。
func Process(input []any, opts Options) ([]any, Stats, error) {
	var stats Stats
	maxBytes, previewRunes := opts.MaxBytes, opts.PreviewRunes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if previewRunes <= 0 {
		previewRunes = DefaultPreviewRunes
	}
	if opts.Dir == "" {
		return input, stats, nil
	}
	var firstErr error
	out := make([]any, len(input))
	for i, raw := range input {
		out[i] = raw
		item, ok := raw.(map[string]any)
		if !ok || !isToolOutputItem(item) {
			continue
		}
		text, ok := textualOutput(item)
		if !ok || len(text) < maxBytes {
			continue
		}
		// 预览重叠守卫：低阈值 + 多字节密集内容（CJK/emoji）时，字节数
		// 可能先于 rune 数过阈 —— 头/尾预览拼起来已覆盖全文，截断只会
		// 把内容重复两遍、omitted 记成负数。没有可省的中段就保留原文。
		if len(safeHead(text, previewRunes))+len(safeTail(text, previewRunes)) >= len(text) {
			continue
		}
		path, err := writeFile(opts.Dir, text)
		if err != nil {
			// 落盘失败：该条保留原文（宁可多花 token 不丢数据）。
			// 判定仍是内容函数，写盘恢复后回到与此前一致的回执。
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		receipt := receipt(text, path, previewRunes)
		replaced := make(map[string]any, len(item))
		for k, v := range item {
			replaced[k] = v
		}
		replaced["output"] = receipt
		out[i] = replaced
		stats.ToolResultsSpilled++
		stats.ToolResultBytesSaved += int64(len(text) - len(receipt))
	}
	return out, stats, firstErr
}

// Cleanup 删除保留期外的落盘文件，返回删除数；目录缺失视为无文件。
func Cleanup(dir string, retention time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-retention)
	removed := 0
	for _, entry := range entries {
		isSpill := strings.HasPrefix(entry.Name(), filePrefix) || strings.HasPrefix(entry.Name(), tmpPrefix)
		if entry.IsDir() || !isSpill {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// StartJanitor 启动保留期清理协程：启动即清一次，之后每小时一次。
// 服务是常驻进程、无停机路径，协程随进程退出。
func StartJanitor(dir string) {
	go func() {
		sweep := func() {
			removed, err := Cleanup(dir, Retention)
			if err != nil {
				log.Printf("[codex-router] spill cleanup failed: %v", err)
				return
			}
			if removed > 0 {
				log.Printf("[codex-router] spill cleanup removed %d expired file(s)", removed)
			}
		}
		sweep()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			sweep()
		}
	}()
}

// writeFile 内容寻址落盘：同内容永远同名，已存在即跳过（幂等）；
// 并发写同一目标时 rename 覆盖也安全（字节相同）。
func writeFile(dir, text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	path := filepath.Join(dir, filePrefix+hex.EncodeToString(sum[:8])+".txt")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return path, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return path, err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return path, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return path, err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return path, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return path, err
	}
	return path, nil
}

// receipt 生成回执：同 (内容, 路径, 预览长度) 产出逐字节相同的结果。
func receipt(value, path string, previewRunes int) string {
	sum := sha256.Sum256([]byte(value))
	head, tail := safeHead(value, previewRunes), safeTail(value, previewRunes)
	omitted := len(value) - len(head) - len(tail)
	return strings.Join([]string{
		fmt.Sprintf("[Tool result truncated by Model Router on first transit: %d bytes, sha256:%s.", len(value), hex.EncodeToString(sum[:])),
		fmt.Sprintf("Full output saved to: %s", path),
		"If the omitted middle matters, Read that file with offset/limit or Grep it instead of reading it in full.",
		"",
		"--- beginning of original result ---",
		head,
		fmt.Sprintf("--- %d bytes omitted · full copy: %s ---", omitted, path),
		tail,
		"--- end of original result ---",
	}, "\n")
}

func isToolOutputItem(item map[string]any) bool {
	t, _ := item["type"].(string)
	return t == "function_call_output" || t == "custom_tool_call_output"
}

// textualOutput 只接受纯文本形态的结果；图像/混合内容原样保留。
func textualOutput(item map[string]any) (string, bool) {
	switch output := item["output"].(type) {
	case string:
		return output, true
	case []any:
		var parts []string
		for _, raw := range output {
			part, ok := raw.(map[string]any)
			if !ok {
				return "", false
			}
			t, _ := part["type"].(string)
			if t != "input_text" && t != "text" {
				return "", false
			}
			text, ok := part["text"].(string)
			if !ok {
				return "", false
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, ""), true
	default:
		return "", false
	}
}

// safeHead/safeTail 按 rune 边界截预览。
func safeHead(value string, previewRunes int) string {
	runes := []rune(value)
	if len(runes) <= previewRunes {
		return value
	}
	return string(runes[:previewRunes])
}

func safeTail(value string, previewRunes int) string {
	runes := []rune(value)
	if len(runes) <= previewRunes {
		return value
	}
	return string(runes[len(runes)-previewRunes:])
}
