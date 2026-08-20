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
	At                   string `json:"at"`
	Model                string `json:"model"`
	Provider             string `json:"provider"`
	Status               int    `json:"status"`
	DurationMs           int64  `json:"durationMs"`
	ResponseStartMs      int64  `json:"responseStartMs,omitempty"`
	FirstTokenMs         int64  `json:"firstTokenMs,omitempty"`
	InputTokens          int64  `json:"inputTokens"`
	OutputTokens         int64  `json:"outputTokens"`
	TotalTokens          int64  `json:"totalTokens"`
	EstimatedInputTokens int64  `json:"estimatedInputTokens,omitempty"`
	StreamAborted        bool   `json:"streamAborted,omitempty"`
	UpstreamIdle         bool   `json:"upstreamIdle,omitempty"`
	EmptyCompletion      bool   `json:"emptyCompletion,omitempty"`
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

// usageRotateBytes 是 usage-events.jsonl 的轮转阈值。Recorder 每次
// 写入都重新打开文件，重命名发生在两次打开之间是安全的；单代归档
// （.1 覆盖旧的 .1）。provider-usage 聚合在文件缺失时返回空视图，
// 轮转瞬间不丢正确性。可变以供测试缩小。
var usageRotateBytes int64 = 8 << 20

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
	// 尺寸轮转：8MB ≈ 数万回合。append-only 无限增长的文件对常驻
	// 服务是慢性的磁盘泄漏，这里在写入前检查并归档单代。
	if info, err := os.Stat(r.path); err == nil && info.Size() >= usageRotateBytes {
		_ = os.Rename(r.path, r.path+".1")
	}
	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	file.Write(append(raw, '\n'))
}

var _ = strings.TrimSpace
