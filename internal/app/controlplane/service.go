package controlplane

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Service 是 `control service` 命令面的生命周期实现：detached 启动
// （终端救急路径，App 之外存活）与 pidfile 停止。App 的
// ServiceSupervisor 子进程托管不经这里 —— 两条路径的所有权边界：
// 谁拉起的进程归谁管，Stop 对 pidfile 进程发 SIGTERM 时两种来源
// 一样能停。
type Service struct {
	// StateDir 是状态目录（pidfile 与 router.log 所在地）。
	StateDir string
	// Port 是服务端口（/health 探测与 serve 参数）。
	Port int
	// BinaryPath 返回 codex-router 自身路径（serve 子命令的 exec 目标）。
	BinaryPath func() (string, error)
	// Out 接收命令面输出（CLI 接 os.Stdout；测试可注入缓冲）。
	Out io.Writer
}

// HealthURL 拼出 /health 探测地址。
func (s Service) HealthURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/health", s.Port)
}

// ProbeHealth 探测本地 /health。超时 2s 与旧 probeURL 对齐 —— 探测
// 语义是"活着吗"，瞬时忙（GC、大请求收尾）不该误判为死。
func (s Service) ProbeHealth() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(s.HealthURL())
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusOK
}

// Running 报告服务是否在答 /health。
func (s Service) Running() bool { return s.ProbeHealth() }

// StartDetached 分离进程拉起 serve：Setsid 脱离会话。serve 的日志经
// --log-file 落 router.log（JSON 行、自带 8MB 单代轮转 —— 终端救急
// 路径与 App 托管路径的轮转语义由此对齐）；stderr 仍重定向进同一文件，
// 兜住 --log-file 接线失败前的启动错误与 Go panic 输出。已在跑时幂等返回。
func (s Service) StartDetached() error {
	if s.ProbeHealth() {
		fmt.Fprintln(s.Out, "service: already running")
		return nil
	}
	exe, err := s.BinaryPath()
	if err != nil {
		return err
	}
	logPath := filepath.Join(s.StateDir, "router.log")
	logFile, err := os.OpenFile(logPath,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		logFile = nil
	}
	cmd := exec.Command(exe, "serve",
		"--state", s.StateDir, "--port", fmt.Sprintf("%d", s.Port),
		"--log-file", logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn serve: %w", err)
	}
	if logFile != nil {
		defer logFile.Close()
	}
	// 等 /health 就绪再报成功 —— 立即返回会让调用方误判。
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if s.ProbeHealth() {
			fmt.Fprintln(s.Out, "service: started")
			return nil
		}
	}
	return fmt.Errorf("serve spawned but /health did not come up within 10s (see router.log)")
}

// StopByPidfile 读状态目录的 pidfile，对进程发 SIGTERM。pidfile 缺失/
// 进程已死都视为已停止（清掉陈旧文件）；服务在答 /health 却没写
// pidfile 时拒绝盲停 —— 进程归它的所有者（App 或终端）管。
func (s Service) StopByPidfile() error {
	pidfile := filepath.Join(s.StateDir, "router.pid")
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		if s.ProbeHealth() {
			return fmt.Errorf("service is answering /health but wrote no pidfile; stop it from its owner (the app or its terminal)")
		}
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		os.Remove(pidfile)
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal %d: %w", pid, err)
	}
	// 等优雅退出（最长 5s），超时不强杀 —— 请求有 10s 的排空预算。
	for i := 0; i < 20; i++ {
		time.Sleep(250 * time.Millisecond)
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			os.Remove(pidfile)
			return nil
		}
	}
	return nil
}
