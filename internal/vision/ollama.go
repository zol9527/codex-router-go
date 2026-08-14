package vision

// Ollama runtime 底座，移植自 ollama-runtime.mjs 的核心：
// 探测、headless 拉起（detached + 日志落盘 + 就绪等待）、
// 进程身份（PID 单独不构成所有权 —— PID 会被复用，pid + 进程启动
// 时刻双匹配才敢 kill）、只停自己启动的守护进程。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultOllamaBaseURL = "http://127.0.0.1:11434"
	ollamaStartTimeout   = 15 * time.Second
	ollamaStopTimeout    = 5 * time.Second
)

// RuntimeState 是 ollama-runtime.json 的形状（所有权记录）。
type RuntimeState struct {
	Version      int    `json:"version"`
	Managed      bool   `json:"managed"`
	PID          int    `json:"pid"`
	ProcessStart string `json:"processIdentity"`
	Command      string `json:"command"`
	BaseURL      string `json:"baseUrl"`
	StartedAt    int64  `json:"startedAt"`
	LogPath      string `json:"logPath"`
}

// ProbeServer 探测 OpenAI 兼容端点：/models 列表 + 视觉模型过滤。
type ProbeResult struct {
	Reachable    bool
	BaseURL      string
	Error        string
	Models       []string
	VisionModels []string
}

// IsVisionModelID 按模型 id 里的视觉家族关键词判定。
func IsVisionModelID(id string) bool {
	lower := strings.ToLower(id)
	for _, marker := range []string{"vl", "vision", "llava", "moondream", "gemma3"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ProbeLocalServer 探测本地推理服务（Ollama / LM Studio / llama.cpp）。
func ProbeLocalServer(ctx context.Context, client *http.Client, baseURL string) ProbeResult {
	base := strings.TrimRight(baseURL, "/")
	result := ProbeResult{BaseURL: base}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		result.Error = "invalid url"
		return result
	}
	resp, err := client.Do(req)
	if err != nil {
		result.Error = "not reachable"
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return result
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		result.Error = "bad response"
		return result
	}
	result.Reachable = true
	for _, entry := range parsed.Data {
		if entry.ID != "" {
			result.Models = append(result.Models, entry.ID)
			if IsVisionModelID(entry.ID) {
				result.VisionModels = append(result.VisionModels, entry.ID)
			}
		}
	}
	return result
}

// ---- 进程身份 ----

// ProcessStartIdentity 返回进程的启动时刻标识（darwin: ps -o lstart；
// linux: /proc/<pid>/stat 的启动滴答）。空串 = 无法验证。
func ProcessStartIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// field 22 是 starttime。
		fields := strings.Fields(string(raw))
		if len(fields) >= 22 {
			return fields[21]
		}
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// StateOwnsProcess：PID 存活且启动身份匹配。
func StateOwnsProcess(state *RuntimeState) bool {
	if state == nil || !state.Managed || state.PID <= 0 {
		return false
	}
	identity := ProcessStartIdentity(state.PID)
	return identity != "" && identity == state.ProcessStart
}

// ---- 状态文件 ----

func runtimeStatePath(stateDir string) string {
	return filepath.Join(stateDir, "ollama-runtime.json")
}

// ReadRuntimeState 读取所有权记录（缺失/损坏返回 nil）。
func ReadRuntimeState(stateDir string) *RuntimeState {
	raw, err := os.ReadFile(runtimeStatePath(stateDir))
	if err != nil {
		return nil
	}
	var state RuntimeState
	if err := json.Unmarshal(raw, &state); err != nil || state.Version != 1 {
		return nil
	}
	return &state
}

func writeRuntimeState(stateDir string, state *RuntimeState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(runtimeStatePath(stateDir), append(raw, '\n'), 0o600)
}

func clearRuntimeState(stateDir string) {
	os.Remove(runtimeStatePath(stateDir))
}

// OllamaCommand 找 ollama 可执行文件（PATH → 常见安装位置）。
func OllamaCommand() string {
	if path, err := exec.LookPath("ollama"); err == nil {
		return path
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{
		"/usr/local/bin/ollama",
		"/opt/homebrew/bin/ollama",
		filepath.Join(home, ".local/bin/ollama"),
		filepath.Join(home, "Applications/Ollama.app/Contents/Resources/ollama"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// EnsureHeadless 确保 loopback 上有一个 Ollama 在跑：
// 已在跑 → 报告（识别是否我们管理的）；没在跑 → detached 拉起
// `ollama serve`（日志 → state/ollama.log），等就绪，记录所有权。
// 若所有权记录无法持久化，宁可杀掉刚启动的进程也不留下一个
// 关机时分不清归属的守护进程。
func EnsureHeadless(ctx context.Context, stateDir, baseURL string) (map[string]any, error) {
	host := hostOf(baseURL)
	if host == "" {
		return nil, fmt.Errorf("the configured local model endpoint is not loopback: %s", baseURL)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	probe := ProbeLocalServer(probeCtx, nil, OllamaRootOf(baseURL))
	if probe.Reachable {
		state := ReadRuntimeState(stateDir)
		managed := StateOwnsProcess(state)
		if state != nil && state.Managed && !managed {
			clearRuntimeState(stateDir) // 记录指向的进程已不在，清掉
		}
		return map[string]any{
			"running": true, "managed": managed, "models": probe.Models,
		}, nil
	}

	command := OllamaCommand()
	if command == "" {
		return nil, fmt.Errorf("Ollama is not installed. Install it from https://ollama.com first")
	}
	logPath := filepath.Join(stateDir, "ollama.log")
	os.MkdirAll(stateDir, 0o700)
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	child := exec.Command(command, "serve")
	child.Env = append(os.Environ(), "OLLAMA_HOST="+host)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detached
	if err := child.Start(); err != nil {
		return nil, fmt.Errorf("starting ollama: %w", err)
	}
	// 不 Wait：守护进程独立于本进程生存。
	go func() { child.Wait() }()

	readyCtx, readyCancel := context.WithTimeout(ctx, ollamaStartTimeout)
	defer readyCancel()
	ready := waitForServer(readyCtx, OllamaRootOf(baseURL))
	if !ready {
		child.Process.Signal(syscall.SIGTERM)
		return nil, fmt.Errorf("Ollama was started headlessly but did not become ready")
	}
	identity := ProcessStartIdentity(child.Process.Pid)
	if identity == "" {
		child.Process.Signal(syscall.SIGTERM)
		return nil, fmt.Errorf("Ollama started, but its process ownership could not be verified")
	}
	if err := writeRuntimeState(stateDir, &RuntimeState{
		Version: 1, Managed: true, PID: child.Process.Pid,
		ProcessStart: identity, Command: command,
		BaseURL: OllamaRootOf(baseURL), StartedAt: time.Now().UnixMilli(), LogPath: logPath,
	}); err != nil {
		child.Process.Signal(syscall.SIGTERM)
		return nil, err
	}
	return map[string]any{
		"running": true, "managed": true, "pid": child.Process.Pid,
	}, nil
}

// hostOf 提取 base URL 的 host:port。
func hostOf(baseURL string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	if i := strings.Index(trimmed, "/"); i >= 0 {
		trimmed = trimmed[:i]
	}
	return trimmed
}

// OllamaRootOf 剥掉 OpenAI 兼容前缀 /v1，得到守护进程根。
func OllamaRootOf(baseURL string) string {
	root := strings.TrimRight(baseURL, "/")
	root = strings.TrimSuffix(root, "/v1")
	return root
}

func waitForServer(ctx context.Context, root string) bool {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		probe := ProbeLocalServer(probeCtx, nil, root)
		cancel()
		if probe.Reachable {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// StopManaged 只停本路由启动的那个守护进程：
// 无记录/记录失效 → 不动（别人的服务器）；PID 复用 → 身份不匹配不动。
func StopManaged(stateDir string) map[string]any {
	state := ReadRuntimeState(stateDir)
	if state == nil || !state.Managed {
		return map[string]any{"stopped": false, "reason": "external-or-unmanaged"}
	}
	if !StateOwnsProcess(state) {
		clearRuntimeState(stateDir)
		return map[string]any{"stopped": false, "reason": "ownership-lost"}
	}
	if err := syscall.Kill(state.PID, syscall.SIGTERM); err != nil {
		clearRuntimeState(stateDir)
		return map[string]any{"stopped": false, "reason": "already-stopped"}
	}
	deadline := time.Now().Add(ollamaStopTimeout)
	for time.Now().Before(deadline) && StateOwnsProcess(state) {
		time.Sleep(100 * time.Millisecond)
	}
	if StateOwnsProcess(state) {
		syscall.Kill(state.PID, syscall.SIGKILL)
	}
	clearRuntimeState(stateDir)
	return map[string]any{"stopped": true, "pid": state.PID}
}
