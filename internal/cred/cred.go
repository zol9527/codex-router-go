// Package cred 按 env → .secret 文件 → macOS Keychain 的顺序解析
// provider 凭据，与原 provider-credentials.mjs 的优先级一致。
package cred

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// Resolver 解析 provider 凭据。Keychain 查询有进程内缓存，
// 因为每次 spawn /usr/bin/security 约 250ms，请求路径不该反复支付。
type Resolver struct {
	State *state.State

	mu          sync.Mutex
	keychainTTL time.Duration
	keychainAt  map[string]time.Time
	keychainVal map[string]string
}

// New 创建解析器。
func New(st *state.State) *Resolver {
	return &Resolver{
		State:       st,
		keychainTTL: 30 * time.Second,
		keychainAt:  map[string]time.Time{},
		keychainVal: map[string]string{},
	}
}

// Resolve 返回凭据值与来源标记（"environment" / "file" / "keychain"）。
// 未配置时返回空串与空来源。
func (r *Resolver) Resolve(p *registry.Provider) (string, string) {
	if p == nil {
		return "", ""
	}
	// 1. 环境变量
	for _, name := range p.Credential.Environment {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, "environment"
		}
	}
	// 2. 状态目录里的 .secret 文件
	if p.Credential.File != "" {
		if v, ok := r.State.ReadCredentialFile(p.Credential.File); ok {
			return v, "file"
		}
	}
	// 3. macOS Keychain（security find-generic-password -s <service> -a default -w）
	if len(p.Credential.KeychainServices) > 0 && runtime.GOOS == "darwin" {
		if v, ok := r.keychain(p.Credential.KeychainServices[0]); ok {
			return v, "keychain"
		}
	}
	return "", ""
}

// keychain 带 TTL 缓存的 Keychain 查询。输出只保留 secret 本身；
// 查询失败（用户拒绝 / 条目不存在）同样缓存，避免每请求重试昂贵 spawn。
func (r *Resolver) keychain(service string) (string, bool) {
	r.mu.Lock()
	if at, ok := r.keychainAt[service]; ok && time.Since(at) < r.keychainTTL {
		v := r.keychainVal[service]
		r.mu.Unlock()
		return v, v != ""
	}
	r.mu.Unlock()

	value := keychainLookup(service)

	r.mu.Lock()
	r.keychainAt[service] = time.Now()
	r.keychainVal[service] = value
	r.mu.Unlock()
	return value, value != ""
}

func keychainLookup(service string) string {
	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", service, "-a", "default", "-w")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
