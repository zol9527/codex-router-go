package vision

// 视觉模型下载，移植自 vision-download.mjs。
// 视觉模型好几个 GB，下载绝不能阻塞发起者（tray 会冻结几分钟、
// 看起来像崩溃）：请求方启动 detached worker 流式读取 Ollama 的
// /api/pull 并把进度写进 vision-download.json，自己立刻返回轮询该文件。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DownloadState 是 vision-download.json 的形状。
type DownloadState struct {
	Version   int    `json:"version"`
	Tag       string `json:"tag"`
	Status    string `json:"status"` // downloading | done | error
	Detail    string `json:"detail"`
	Percent   int    `json:"percent"`
	Adopted   bool   `json:"adopted,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

func downloadStatePath(stateDir string) string {
	return stateDir + "/vision-download.json"
}

// ReadDownload 读取下载状态（缺失/半写 → nil，即"无进行中下载"）。
func ReadDownload(stateDir string) *DownloadState {
	raw, err := os.ReadFile(downloadStatePath(stateDir))
	if err != nil {
		return nil
	}
	var state DownloadState
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != 1 {
		return nil
	}
	return &state
}

// WriteDownload 原子写状态（0600）。
func WriteDownload(stateDir string, state DownloadState) error {
	state.UpdatedAt = time.Now().UnixMilli()
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	temporary := downloadStatePath(stateDir) + ".tmp"
	if err := os.WriteFile(temporary, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, downloadStatePath(stateDir))
}

// ProgressTracker 把 Ollama 按层报告的进度（每层 completed/total，
// 新层开始时跳回零）汇成一个只前进的总百分比。
type ProgressTracker struct {
	layers map[string]progressLayer
}

type progressLayer struct {
	completed float64
	total     float64
}

// NewProgressTracker 创建跟踪器。
func NewProgressTracker() *ProgressTracker {
	return &ProgressTracker{layers: map[string]progressLayer{}}
}

// Update 吸收一个 pull 事件；无有效数字返回 -1。
func (p *ProgressTracker) Update(event map[string]any) int {
	key, _ := event["digest"].(string)
	if key == "" {
		key, _ = event["status"].(string)
	}
	if key == "" {
		return -1
	}
	total, _ := event["total"].(float64)
	if total <= 0 {
		return -1
	}
	completed, _ := event["completed"].(float64)
	p.layers[key] = progressLayer{completed: completed, total: total}
	var sumCompleted, sumTotal float64
	for _, layer := range p.layers {
		sumCompleted += layer.completed
		sumTotal += layer.total
	}
	if sumTotal <= 0 {
		return -1
	}
	percent := int(sumCompleted / sumTotal * 100)
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return percent
}

// StreamOllamaPull 流式执行 POST /api/pull，逐事件回调进度，
// 直到 Ollama 确认 success。error 事件直接失败。
func StreamOllamaPull(ctx context.Context, client *http.Client, tag, baseURL string,
	onProgress func(detail string, percent int)) error {

	root := OllamaRootOf(baseURL)
	body, _ := json.Marshal(map[string]any{"model": tag, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/pull",
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach Ollama. Is it installed and running?")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Ollama refused the download (HTTP %d)", resp.StatusCode)
	}
	tracker := NewProgressTracker()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	sawSuccess := false
	lastPercent := -1
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue // Ollama 只发 JSON 对象，其他是噪音
		}
		if errText, ok := event["error"].(string); ok && errText != "" {
			return fmt.Errorf("%s", errText)
		}
		if status, _ := event["status"].(string); status == "success" {
			sawSuccess = true
		}
		detail, _ := event["status"].(string)
		if detail == "" {
			detail = "downloading"
		}
		percent := tracker.Update(event)
		if percent < 0 {
			percent = lastPercent
		}
		lastPercent = percent
		if onProgress != nil {
			onProgress(detail, percent)
		}
	}
	if !sawSuccess {
		return fmt.Errorf("the download ended before Ollama confirmed success")
	}
	return nil
}

// RunDetachedPull 是 detached worker 的主体：写进度文件直到完成。
// 下载完成不等于被选为引擎 —— 新拉的模型未测量，悄悄顶掉一个已知
// 可靠的读图器是把"准确转录"换成"可能编造"而无人在场同意。只有
// 当前根本没有引擎时才采用（首启场景：任何读图器都胜过没有）。
func RunDetachedPull(stateDir, tag, baseURL string) error {
	started := time.Now().UnixMilli()
	WriteDownload(stateDir, DownloadState{
		Version: 1, Tag: tag, StartedAt: started,
		Status: "downloading", Detail: "starting", Percent: 0,
	})
	lastPercent := -1
	lastDetail := ""
	err := StreamOllamaPull(context.Background(), nil, tag, baseURL,
		func(detail string, percent int) {
			shown := percent
			if shown < 0 {
				shown = lastPercent
			}
			if shown == lastPercent && detail == lastDetail {
				return
			}
			lastPercent = shown
			lastDetail = detail
			if shown < 0 {
				shown = 0
			}
			WriteDownload(stateDir, DownloadState{
				Version: 1, Tag: tag, StartedAt: started,
				Status: "downloading", Detail: detail, Percent: shown,
			})
		})
	if err != nil {
		percent := lastPercent
		if percent < 0 {
			percent = 0
		}
		WriteDownload(stateDir, DownloadState{
			Version: 1, Tag: tag, StartedAt: started,
			Status: "error", Detail: "failed", Percent: percent, Error: err.Error(),
		})
		return err
	}
	// 模型确实落盘后才可能成为读图器 —— 失败/取消的下载绝不把
	// 桥指向一个不存在的模型。
	adopt := false
	if settings, configured := ReadSettings(stateDir); !configured ||
		settings.Enabled == nil || *settings.Enabled == false || settings.Engine == "" {
		enabled := true
		next := Settings{Enabled: &enabled, Engine: LocalEngineSlug, LocalModel: tag}
		if baseURL != "" && baseURL != DefaultLocalVisionBaseURL {
			next.LocalBaseURL = baseURL
		}
		if raw, err := json.Marshal(next); err == nil {
			os.MkdirAll(stateDir, 0o700)
			os.WriteFile(stateDir+"/vision-bridge.json", append(raw, '\n'), 0o600)
		}
		adopt = true
	}
	detail := "downloaded"
	if adopt {
		detail = "ready"
	}
	return WriteDownload(stateDir, DownloadState{
		Version: 1, Tag: tag, StartedAt: started,
		Status: "done", Detail: detail, Adopted: adopt, Percent: 100,
	})
}

// StartDetachedPull 启动 detached worker（重新 exec 自身的隐藏子命令）
// 并立即返回。caller 轮询 pull-status。
func StartDetachedPull(selfBinary, stateDir, tag, baseURL string) error {
	cmd := exec.Command(selfBinary, "__vision-pull-worker", "--state", stateDir, "--base", baseURL, tag)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { cmd.Wait() }() // 回收僵尸，不等待
	return nil
}
