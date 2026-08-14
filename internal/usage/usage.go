// Package usage 是 usage-events.jsonl 管道：每回合 append 一行 JSON，
// provider-usage 聚合读回。字段形状与 Node 版一致（tray 沿用）。
package usage

import (
	"bufio"
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

// ProviderUsage 是聚合视图（tray 的用量卡片）。
type ProviderUsage struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Turns    int     `json:"turns"`
	Errors   int     `json:"errors"`
	Tokens   int64   `json:"totalTokens"`
	TTFTMs   float64 `json:"medianFirstTokenMs,omitempty"`
}

// Aggregate 聚合近 90 天的用量（每 provider+model 一行）。
func Aggregate(stateDir string) []ProviderUsage {
	path := filepath.Join(stateDir, "usage-events.jsonl")
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	type key struct{ provider, model string }
	type agg struct {
		turns, errors int
		tokens        int64
		ttfts         []int64
	}
	aggregates := map[key]*agg{}
	cutoff := time.Now().AddDate(0, 0, -90).Format(time.RFC3339)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.At < cutoff {
			continue
		}
		k := key{event.Provider, event.Model}
		entry := aggregates[k]
		if entry == nil {
			entry = &agg{}
			aggregates[k] = entry
		}
		entry.turns++
		if event.Status >= 400 || event.Status == 0 {
			entry.errors++
		}
		entry.tokens += event.TotalTokens
		if event.FirstTokenMs > 0 {
			entry.ttfts = append(entry.ttfts, event.FirstTokenMs)
		}
	}
	var out []ProviderUsage
	for k, entry := range aggregates {
		row := ProviderUsage{
			Provider: k.provider, Model: k.model,
			Turns: entry.turns, Errors: entry.errors, Tokens: entry.tokens,
		}
		if len(entry.ttfts) > 0 {
			var sum int64
			for _, v := range entry.ttfts {
				sum += v
			}
			row.TTFTMs = float64(sum) / float64(len(entry.ttfts))
		}
		out = append(out, row)
	}
	return out
}

var _ = strings.TrimSpace
