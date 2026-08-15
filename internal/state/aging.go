// Package state 的 tool-result aging 设置与累计统计。
//
// 门控语义：Go 版自移植起 aging 一直常开 —— 引入开关时以"开"为
// 默认，与既有行为连续（原 Node 版默认关，但那是首次引入实验特性时
// 的选择）。统计按"实际发生老化"的回合累计（结果数/字节/估算 token），
// App 设置页的节省摘要把这些数读给操作者看。
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// AgingStats 是累计的老化统计。
type AgingStats struct {
	Requests             int   `json:"requests"`
	ResultsAged          int   `json:"resultsAged"`
	BytesSaved           int64 `json:"bytesSaved"`
	EstimatedTokensSaved int64 `json:"estimatedTokensSaved"`
}

type toolResultAgingState struct {
	Version int         `json:"version"`
	Enabled *bool       `json:"enabled,omitempty"` // nil = 未设置 → 默认开
	Stats   AgingStats  `json:"stats"`
}

// 状态文件写入有进程内互斥：serve 进程每个发生老化的回合写一次，
// control 命令也可能同时写开关 —— 串行化避免丢更新。
var agingWriteMu sync.Mutex

func toolResultAgingPath(stateDir string) string {
	return filepath.Join(stateDir, "tool-result-aging.json")
}

// ReadToolResultAging 返回开关（默认开）与累计统计。
func ReadToolResultAging(stateDir string) (bool, AgingStats) {
	raw, err := os.ReadFile(toolResultAgingPath(stateDir))
	if err != nil {
		return true, AgingStats{}
	}
	var parsed toolResultAgingState
	if json.Unmarshal(raw, &parsed) != nil {
		return true, AgingStats{} // 坏文件不改变默认行为
	}
	enabled := true
	if parsed.Enabled != nil {
		enabled = *parsed.Enabled
	}
	return enabled, parsed.Stats
}

// SetToolResultAgingEnabled 切换开关（统计保留）。
func SetToolResultAgingEnabled(stateDir string, enabled bool) error {
	agingWriteMu.Lock()
	defer agingWriteMu.Unlock()
	_, stats := ReadToolResultAgingUnlocked(stateDir)
	flag := enabled
	return writeToolResultAging(stateDir, &flag, stats)
}

// RecordAgingStats 累加一轮实际发生的 aging（aged>0 时调用）。
func RecordAgingStats(stateDir string, delta AgingStats) error {
	agingWriteMu.Lock()
	defer agingWriteMu.Unlock()
	enabled, stats := ReadToolResultAgingUnlocked(stateDir)
	stats.Requests++
	stats.ResultsAged += delta.ResultsAged
	stats.BytesSaved += delta.BytesSaved
	stats.EstimatedTokensSaved += delta.EstimatedTokensSaved
	var flag *bool
	if !enabled {
		flag = &enabled // 显式记 false，否则省略字段即默认开
	}
	return writeToolResultAging(stateDir, flag, stats)
}

// ReadToolResultAgingUnlocked 是内部读（调用方持锁）。
func ReadToolResultAgingUnlocked(stateDir string) (bool, AgingStats) {
	return ReadToolResultAging(stateDir)
}

func writeToolResultAging(stateDir string, enabled *bool, stats AgingStats) error {
	raw, err := json.MarshalIndent(toolResultAgingState{Version: 1, Enabled: enabled, Stats: stats}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp := toolResultAgingPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, toolResultAgingPath(stateDir))
}

// ToolResultAgingSnapshot 是 control --json 的 toolResultAging 块。
// enabled 非可选（Swift 解码器）；stats 总是发出（零值即"还没省过"）。
func ToolResultAgingSnapshot(stateDir string) map[string]any {
	enabled, stats := ReadToolResultAging(stateDir)
	return map[string]any{
		"enabled": enabled,
		"stats": map[string]any{
			"requests":             stats.Requests,
			"resultsAged":          stats.ResultsAged,
			"bytesSaved":           stats.BytesSaved,
			"estimatedTokensSaved": stats.EstimatedTokensSaved,
		},
	}
}
