// codex-router 是原 Node + LiteLLM 栈的 Go 单二进制重写。
// serve 子命令启动 4202 上的 router 服务（M1 范围）。
package main

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
	"syscall"
	"time"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/server"
	"github.com/loyd/codex-router/internal/state"
)

// version 在构建时通过 -ldflags 注入。
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
		os.Exit(64)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "version", "--version":
		fmt.Println("codex-router " + version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(64)
	}
	if err != nil {
		log.Fatalf("[codex-router] fatal: %v", err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `codex-router — Go rewrite of the codex-router service

Usage:
  codex-router serve [--port N] [--state DIR] [--config DIR]
  codex-router version
`)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", defaultPort(), "listen port")
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	configDir := fs.String("config", "", "registry config directory (default: <repo>/config)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *configDir == "" {
		*configDir = defaultConfigDir()
	}
	reg, err := registry.Load(*configDir)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	st, err := state.Open(*stateDir)
	if err != nil {
		return err
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
	log.Printf("[codex-router] listening on %s (version %s)", listenAddr, version)

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}()
	return httpServer.Serve(listener)
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

// defaultConfigDir 定位仓库内的 config/ 目录：可执行文件旁，或源码树。
func defaultConfigDir() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "..", "config")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	// 源码树运行（go run / go test）：从当前目录向上找 config/。
	dir, err := os.Getwd()
	if err != nil {
		return "config"
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "config")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "config"
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
