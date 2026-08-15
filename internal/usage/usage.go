// Package usage 是 usage-events.jsonl 管道：每回合 append 一行 JSON，
// provider-usage 聚合读回。字段形状与 Node 版一致（tray 沿用）。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Event 是一回合的计量记录。零值字段不序列化（保持 JSONL 紧凑，
// 与 Node 版的条件展开一致）。
type Event struct {
	At                     string `json:"at"`
	Model                  string `json:"model"`
	Provider               string `json:"provider"`
	Status                 int    `json:"status"`
	DurationMs             int64  `json:"durationMs"`
	ResponseStartMs        int64  `json:"responseStartMs,omitempty"`
	FirstTokenMs           int64  `json:"firstTokenMs,omitempty"`
	Retries                int    `json:"retries,omitempty"`
	InputTokens            int64  `json:"inputTokens"`
	OutputTokens           int64  `json:"outputTokens"`
	TotalTokens            int64  `json:"totalTokens"`
	CachedInputTokens      int64  `json:"cachedInputTokens,omitempty"`
	EstimatedInputTokens   int64  `json:"estimatedInputTokens,omitempty"`
	StreamAborted          bool   `json:"streamAborted,omitempty"`
	EmptyCompletion        bool   `json:"emptyCompletion,omitempty"`
	EmptyCompletionRetried bool   `json:"emptyCompletionRetried,omitempty"`
	ToolResultsAged        int    `json:"toolResultsAged,omitempty"`
	ToolResultBytesSaved   int64  `json:"toolResultBytesSaved,omitempty"`
}

// Recorder 追加写入 JSONL（进程内串行，0600）。
type Recorder struct {
	mu   sync.Mutex
	path string
}

// NewRecorder 创建记录器。
func NewRecorder(stateDir string) *Recorder {
	return &Recorder{path: filepath.Join(stateDir, "usage-events.jsonl")}
}

// Path 返回 JSONL 文件路径。
func (r *Recorder) Path() string { return r.path }

// Record 追加一条事件。失败只影响计量，绝不影响请求路径。
func (r *Recorder) Record(event Event) {
	if r == nil {
		return
	}
	if event.At == "" {
		event.At = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	file.Write(append(raw, '\n'))
}

var _ = strings.TrimSpace
