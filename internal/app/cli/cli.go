// Package cli 承载 codex-router 的全部命令实现与分发。CLI 与 HTTP
// server、controlplane 一样是对外表面，归 app 层；cmd/codex-router
// 只剩版本注入（构建时 -X main.version 打在 package main 上）与
// 进程入口。Main 是本包唯一导出面 —— 进程退出码也归它管。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/loyd/codex-router/internal/app/server"
	"github.com/loyd/codex-router/internal/domain/cred"
	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/state"
	"github.com/loyd/codex-router/internal/domain/usage"
	"github.com/loyd/codex-router/internal/lib/logx"
)

// Main 是二进制的完整入口：分发子命令并持有进程退出语义。
// version 来自构建注入（见 cmd/codex-router/main.go）。
func Main(version string) {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(64)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:], version)
	case "install":
		err = cmdInstall(os.Args[2:])
	case "uninstall":
		err = cmdUninstall(os.Args[2:])
	case "discover":
		err = cmdDiscover(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "control":
		err = cmdControl(os.Args[2:], version)
	case "shim":
		if len(os.Args) < 3 {
			err = fmt.Errorf("shim requires install|uninstall|status")
		} else {
			switch os.Args[2] {
			case "install":
				err = cmdShimInstall(os.Args[3:])
			case "uninstall":
				err = cmdShimUninstall(os.Args[3:])
			case "status":
				err = cmdShimStatus(os.Args[3:])
			default:
				err = fmt.Errorf("unknown shim action %q", os.Args[2])
			}
		}
	case "version", "--version":
		fmt.Println("codex-router " + version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(64)
	}
	if err != nil {
		// fatal 双写：logx 文件模式下 stderr 无守护（serve 由 supervisor
		// 拉起时不重定向），退出前的错误必须同时落到 stderr 保留线索。
		logx.Error("fatal", "error", err)
		fmt.Fprintf(os.Stderr, "[codex-router] fatal: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `codex-router — Go rewrite of the codex-router service

Usage:
  codex-router serve [--port N] [--state DIR] [--config DIR]
                     [--log-file PATH] [--log-level debug|info|warn|error]
  codex-router install [--providers ID,ID] [--dry-run]
  codex-router uninstall [--purge]   (removes plist, binary, config blocks;
                                     --purge also destroys state)
  codex-router doctor
  codex-router control --json | control SERVICE ACTION | ...
  codex-router shim install|uninstall|status
  codex-router version
`)
}

func cmdServe(args []string, version string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", defaultPort(), "listen port")
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	configDir := fs.String("config", "", "registry config directory override (default: embedded registry)")
	logFile := fs.String("log-file", envOr("CODEX_ROUTER_LOG_FILE", ""),
		"log file path (JSON lines, 8MB single-generation rotation); empty = stderr text")
	logLevel := fs.String("log-level", envOr("CODEX_ROUTER_LOG_LEVEL", "info"),
		"minimum log level: debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 日志最先接线：state/registry 的装载错误也要进管道，而不是
	// 只在 stderr 上一闪而过（supervisor 场景 stderr 无人看）。
	if err := logx.Setup(logx.Config{Level: *logLevel, File: *logFile}); err != nil {
		return err
	}
	defer logx.Close()

	st, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	// 注册表 = 内嵌 + user-models.json 覆盖层（动态注册的模型）。
	reg, err := registry.LoadWithOverlay(st.Dir, *configDir)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	// Go 版不再依赖内部 hop，但 caller key 是 Codex config.toml 认证的
	// 全部依据 —— 缺失必须在此失败而不是在每个请求上。
	if _, err := st.CallerKey(); err != nil {
		return err
	}

	srv, err := server.New(server.Options{
		State:       st,
		Registry:    reg,
		Credentials: cred.New(st),
		ListenAddr:  fmt.Sprintf("127.0.0.1:%d", *port),
		NativeBase:  envOr("CODEX_NATIVE_BASE_URL", "https://chatgpt.com/backend-api/codex"),
		// WS 透传回滚开关：设 0 退回"升级握手一律 426、全部走 HTTP"。
		DisableWebSocketPassthrough: os.Getenv("CODEX_WS_PASSTHROUGH") == "0",
		Usage:                       usage.NewRecorder(st.Dir),
		RateLimits:                  usage.NewRateLimitStore(st.Dir),
		// 上游 fail-fast 三闸 + 慢请求可见性闸（秒；0=默认档，设 0 秒
		// 之外的关闭值见 README「上游超时与看门狗」）：响应头超时 /
		// SSE 流空闲看门狗 / WS 管道方向相关静默看门狗 / 慢请求日志。
		UpstreamHeaderTimeout: envDurationSec("CODEX_ROUTER_HEADER_TIMEOUT_SEC", server.DefaultUpstreamHeaderTimeout),
		UpstreamIdleTimeout:   envDurationSec("CODEX_ROUTER_IDLE_TIMEOUT_SEC", server.DefaultUpstreamIdleTimeout),
		WSSilentTimeout:       envDurationSec("CODEX_ROUTER_WS_SILENT_TIMEOUT_SEC", server.DefaultWSSilentTimeout),
		WSKeepaliveInterval:   envDurationSec("CODEX_ROUTER_WS_KEEPALIVE_SEC", server.DefaultWSKeepaliveInterval),
		SlowRequestLogDelay:   envDurationSec("CODEX_ROUTER_SLOW_REQUEST_LOG_SEC", server.DefaultSlowRequestLogDelay),
	})
	if err != nil {
		return err
	}
	server.Version = version

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// 思考型模型的长流由请求上下文管理，不用全局超时。
	}
	listenAddr := fmt.Sprintf("127.0.0.1:%d", *port)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && errors.Is(opErr.Err, syscall.EADDRINUSE) {
			return fmt.Errorf("cannot listen: %s is already in use; stop the other router first", listenAddr)
		}
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	// pidfile 写在成功 listen 之后 —— bind 失败的实例绝不能覆盖健康
	// 实例的 pidfile。control service stop 靠它找到进程。
	pidfile := filepath.Join(st.Dir, "router.pid")
	if err := os.WriteFile(pidfile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		logx.Error("pidfile write failed", "error", err)
	}
	logx.Info("listening", "addr", listenAddr, "version", version)

	done := make(chan os.Signal, 2)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}()
	// SIGUSR1 = 热重载注册表（覆盖层变化后 control models 发来）。
	// 换的是 Server 里的指针，请求路径只读，无需停机。
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		for range usr1 {
			next, err := registry.LoadWithOverlay(st.Dir, *configDir)
			if err != nil {
				logx.Error("registry reload failed, keeping previous", "error", err)
				continue
			}
			srv.SetRegistry(next)
			logx.Info("registry reloaded", "models", len(next.Models))
		}
	}()
	err = httpServer.Serve(listener)
	os.Remove(pidfile)
	// Shutdown 触发的 ErrServerClosed 是正常停机路径（SIGTERM/SIGINT），
	// 渲染成 fatal 会把每次正常重启都变成"事故"。
	if errors.Is(err, http.ErrServerClosed) {
		logx.Info("server stopped")
		return nil
	}
	return err
}

func defaultPort() int {
	for _, name := range []string{"MODEL_ROUTER_PORT", "CODEX_ROUTER_PORT"} {
		if v := os.Getenv(name); v != "" {
			var port int
			if _, err := fmt.Sscanf(v, "%d", &port); err == nil && port > 0 && port < 65536 {
				return port
			}
		}
	}
	return 4202
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// envDurationSec 解析秒数 env：缺省 → 默认档；"0" → 负一（关闭）；
// 正整数 → 指定档。非法值按缺省处理（fail-open 到默认，启动不炸）。
func envDurationSec(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	sec, err := strconv.Atoi(v)
	if err != nil || sec < 0 {
		return def
	}
	if sec == 0 {
		return -1 // 关闭（server.resolveTimeout 负值语义）
	}
	return time.Duration(sec) * time.Second
}
