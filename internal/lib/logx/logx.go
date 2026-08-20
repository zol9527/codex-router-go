// Package logx 是进程级日志门面：标准库 slog 的薄封装。
//
// 两种输出形态由 Setup 决定：
//   - stderr 模式（默认）：TextHandler，前台调试肉眼可读；
//   - 文件模式（--log-file）：JSONHandler + 尺寸轮转（单代 .1 归档），
//     机器可解析，jq/grep 友好。
//
// Setup 同时 slog.SetDefault —— 标准库 log 与第三方库（net/http 的
// 内部错误日志）自动桥接到同一 handler，全进程日志单点收敛。
//
// 并发约定：包级函数全部并发安全；Swap 仅供测试在串行语境下换装。
package logx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Config 是 Setup 的全部开关。
type Config struct {
	// Level 是最低输出级别：debug|info|warn|error。空串 = info。
	Level string
	// File 是日志文件路径：空 = stderr(text)；非空 = 该文件(JSON)。
	File string
	// RotateBytes 是文件模式的尺寸轮转阈值，超限归档为 .1（单代，
	// 覆盖旧归档）。0 = 默认 8MB。测试可缩小。
	RotateBytes int64
}

// DefaultRotateBytes 与 usage-events.jsonl 的轮转阈值对齐：router 日志
// 日均约 4 行/分钟（峰值 12），8MB ≈ 数月容量，单代归档足够。
const DefaultRotateBytes int64 = 8 << 20

// current 是当前进程 logger。Swap/Setup 原子替换，读侧无锁。
var current atomic.Pointer[slog.Logger]

// closer 保存文件模式下的句柄，Close 时释放。
var (
	closerMu sync.Mutex
	closer   io.Closer
)

// Setup 初始化进程日志。stderr 模式永不失败；文件模式在目标不可写时
// 返回错误（显式指定了落盘位置却打不开，应让调用方决定去留，而不是
// 静默降级丢日志）。重复调用以后者为准（测试串行换装场景）。
func Setup(cfg Config) error {
	level := slog.LevelInfo
	switch cfg.Level {
	case "", "info":
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return fmt.Errorf("logx: unknown level %q (want debug|info|warn|error)", cfg.Level)
	}
	opts := &slog.HandlerOptions{Level: level}

	var (
		logger *slog.Logger
		cls    io.Closer
	)
	if cfg.File == "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, opts))
	} else {
		maxBytes := cfg.RotateBytes
		if maxBytes <= 0 {
			maxBytes = DefaultRotateBytes
		}
		w, err := newRotatingWriter(cfg.File, maxBytes)
		if err != nil {
			return fmt.Errorf("logx: open log file: %w", err)
		}
		logger = slog.New(slog.NewJSONHandler(w, opts))
		cls = w
	}

	closerMu.Lock()
	old := closer
	closer = cls
	closerMu.Unlock()
	if old != nil {
		old.Close()
	}
	slog.SetDefault(logger)
	current.Store(logger)
	return nil
}

// Close 释放文件句柄（stderr 模式下是空操作）。serve 退出路径调用。
func Close() error {
	closerMu.Lock()
	defer closerMu.Unlock()
	if closer == nil {
		return nil
	}
	err := closer.Close()
	closer = nil
	return err
}

// Default 返回当前进程 logger（Setup 之前为零配置 stderr text）。
func Default() *slog.Logger {
	if l := current.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// With 派生带预置 kv 的子 logger —— 请求/管道作用域的用法：
//
//	log := logx.With("req", logx.NewID(), "model", slug)
//	log.Info("request done", "status", 200)
func With(kv ...any) *slog.Logger {
	return Default().With(kv...)
}

// Info/Warn/Error/Debug 打到当前进程 logger。
func Info(msg string, kv ...any)  { Default().Log(context.Background(), slog.LevelInfo, msg, kv...) }
func Warn(msg string, kv ...any)  { Default().Log(context.Background(), slog.LevelWarn, msg, kv...) }
func Error(msg string, kv ...any) { Default().Log(context.Background(), slog.LevelError, msg, kv...) }
func Debug(msg string, kv ...any) { Default().Log(context.Background(), slog.LevelDebug, msg, kv...) }

// Swap 换装测试 logger，返回还原闭包。仅测试使用 —— 生产路径的
// logger 替换只应经 Setup。
func Swap(l *slog.Logger) (restore func()) {
	prev := current.Swap(l)
	slog.SetDefault(l)
	return func() {
		var back *slog.Logger
		if prev != nil {
			back = prev
		}
		if back == nil {
			// Setup 之前 Swap 的还原：回到零配置默认。
			back = slog.New(slog.NewTextHandler(io.Discard, nil))
			slog.SetDefault(back)
			current.Store(back)
			return
		}
		current.Store(back)
		slog.SetDefault(back)
	}
}

// NewID 生成 8 位 hex 短 id：请求/管道日志的关联键。8 hex（32 bit）
// 在单进程生命期内的碰撞概率对日志关联用途可忽略；真需要唯一性
// 的地方不在这里。
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见（熵池枯竭）；退化成时间戳低 32 bit，
		// 关联能力仍在，只是理论碰撞率升高。
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// rotatingWriter 是追加写 + 尺寸轮转的文件 writer：写前比对缓存尺寸，
// 超限则 close → rename(.1) → reopen（单代归档，覆盖旧 .1）。rename
// 发生在本 writer 的锁内，自身无竞态；其他进程（Swift supervisor 也
// append 同一文件）的句柄在 rename 后指向旧 inode —— 由对方自行重开，
// 本 writer 只保证自己的正确性。
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64 // 已写尺寸缓存，rotate 后清零
}

func newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &rotatingWriter{path: path, maxBytes: maxBytes, file: file, size: info.Size()}, nil
}

// Write 追加一行（slog 每条日志一次 Write，行级原子由 O_APPEND 保证）。
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size >= w.maxBytes {
		w.rotateLocked()
	}
	if w.file == nil {
		// rotate 重开失败（目录被移走等）：丢弃本行而不是 panic ——
		// 日志管道绝不能拖垮请求路径。下次 Write 会再试。
		return len(p), nil
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 关闭当前句柄。之后的 Write 丢弃（进程已在退出路径）。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotateLocked 归档当前文件并重开。失败时置 file=nil 降级为丢弃写入。
func (w *rotatingWriter) rotateLocked() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		return
	}
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	w.file = file
	w.size = 0
}
