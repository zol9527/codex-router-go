// Package state 的 tool-result spill 设置与累计统计。
//
// 取代旧 tool-result aging（首过境确定性截断，机制见 internal/spill）。
// 门控语义延续 aging：默认开（nil = 未设置 → 默认开），开关与阈值
// 写状态文件、serve 进程逐请求读取 —— 改完下一回合即生效，无需重启。
// 统计按"实际发生截断"的回合累计，App 设置页的节省摘要把这些数
// 读给操作者看。
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/loyd/codex-router/internal/spill"
)

// SpillStats 是累计的截断统计。
type SpillStats struct {
	Requests             int   `json:"requests"`
	ResultsSpilled       int   `json:"resultsSpilled"`
	BytesSaved           int64 `json:"bytesSaved"`
	EstimatedTokensSaved int64 `json:"estimatedTokensSaved"`
}

type toolResultSpillState struct {
	Version  int        `json:"version"`
	Enabled  *bool      `json:"enabled,omitempty"`  // nil = 未设置 → 默认开
	MaxBytes *int       `json:"maxBytes,omitempty"` // nil = 未设置 → spill.DefaultMaxBytes
	Stats    SpillStats `json:"stats"`
}

// 状态文件写入有进程内互斥：serve 进程每个发生截断的回合写一次，
// control 命令也可能同时写开关 —— 串行化避免丢更新。
var spillWriteMu sync.Mutex

func toolResultSpillPath(stateDir string) string {
	return filepath.Join(stateDir, "tool-result-spill.json")
}

// ReadToolResultSpill 返回开关（默认开）、阈值（默认 spill.DefaultMaxBytes）
// 与累计统计。坏文件不改变默认行为。
func ReadToolResultSpill(stateDir string) (bool, int, SpillStats) {
	raw, err := os.ReadFile(toolResultSpillPath(stateDir))
	if err != nil {
		return true, spill.DefaultMaxBytes, SpillStats{}
	}
	var parsed toolResultSpillState
	if json.Unmarshal(raw, &parsed) != nil {
		return true, spill.DefaultMaxBytes, SpillStats{}
	}
	enabled, maxBytes := true, spill.DefaultMaxBytes
	if parsed.Enabled != nil {
		enabled = *parsed.Enabled
	}
	if parsed.MaxBytes != nil && *parsed.MaxBytes > 0 {
		maxBytes = *parsed.MaxBytes
	}
	return enabled, maxBytes, parsed.Stats
}

// SetToolResultSpillEnabled 切换开关（统计与阈值保留）。
func SetToolResultSpillEnabled(stateDir string, enabled bool) error {
	spillWriteMu.Lock()
	defer spillWriteMu.Unlock()
	_, maxBytes, stats := readToolResultSpillUnlocked(stateDir)
	flag := enabled
	return writeToolResultSpill(stateDir, &flag, maxBytes, stats)
}

// SetToolResultSpillMaxBytes 调整截断阈值（字节；下限 1024，防误配零值）。
func SetToolResultSpillMaxBytes(stateDir string, maxBytes int) error {
	if maxBytes < 1024 {
		return fmt.Errorf("spill max-bytes floor is 1024 bytes")
	}
	spillWriteMu.Lock()
	defer spillWriteMu.Unlock()
	enabled, _, stats := readToolResultSpillUnlocked(stateDir)
	var flag *bool
	if !enabled {
		flag = &enabled // 显式记 false，否则省略字段即默认开
	}
	return writeToolResultSpill(stateDir, flag, &maxBytes, stats)
}

// RecordSpillStats 累加一轮实际发生的截断（spilled>0 时调用）。
func RecordSpillStats(stateDir string, delta SpillStats) error {
	spillWriteMu.Lock()
	defer spillWriteMu.Unlock()
	enabled, maxBytes, stats := readToolResultSpillUnlocked(stateDir)
	stats.Requests++
	stats.ResultsSpilled += delta.ResultsSpilled
	stats.BytesSaved += delta.BytesSaved
	stats.EstimatedTokensSaved += delta.EstimatedTokensSaved
	var flag *bool
	if !enabled {
		flag = &enabled
	}
	return writeToolResultSpill(stateDir, flag, maxBytes, stats)
}

// readToolResultSpillUnlocked 是内部读（调用方持锁）。
func readToolResultSpillUnlocked(stateDir string) (bool, *int, SpillStats) {
	enabled, maxBytes, stats := ReadToolResultSpill(stateDir)
	stored := maxBytes
	return enabled, &stored, stats
}

func writeToolResultSpill(stateDir string, enabled *bool, maxBytes *int, stats SpillStats) error {
	raw, err := json.MarshalIndent(toolResultSpillState{
		Version:  1,
		Enabled:  enabled,
		MaxBytes: maxBytes,
		Stats:    stats,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp := toolResultSpillPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, toolResultSpillPath(stateDir))
}

// ToolResultSpillSnapshot 是 control --json 的 toolResultSpill 块。
// enabled/maxBytes 非可选（Swift 解码器）；stats 总是发出（零值即"还没省过"）。
func ToolResultSpillSnapshot(stateDir string) map[string]any {
	enabled, maxBytes, stats := ReadToolResultSpill(stateDir)
	return map[string]any{
		"enabled":  enabled,
		"maxBytes": maxBytes,
		"stats": map[string]any{
			"requests":             stats.Requests,
			"resultsSpilled":       stats.ResultsSpilled,
			"bytesSaved":           stats.BytesSaved,
			"estimatedTokensSaved": stats.EstimatedTokensSaved,
		},
	}
}
