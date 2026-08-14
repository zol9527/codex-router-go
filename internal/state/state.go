// Package state 管理 ~/.codex/codex-router 状态目录：两个 secret、
// provider 选择、凭据文件。目录格式与原 Node 栈完全一致，
// 切换日 caller-secret / internal-secret / *.secret 原位复用。
package state

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// State 持有已解析的状态目录路径与内容。
type State struct {
	Dir string
}

// DefaultDir 返回默认状态目录（可用 CODEX_ROUTER_STATE_DIR 覆盖）。
func DefaultDir() string {
	if env := os.Getenv("CODEX_ROUTER_STATE_DIR"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex-router-state"
	}
	return filepath.Join(home, ".codex", "codex-router")
}

// Open 确认状态目录可用并返回 State 句柄。
func Open(dir string) (*State, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &State{Dir: dir}, nil
}

// readSecretFile 读取一个去除首尾空白后的单行文件。
func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// CallerKey 返回 Codex 侧集成使用的 caller capability（URL 路径 secret）。
func (s *State) CallerKey() (string, error) {
	key, err := readSecretFile(filepath.Join(s.Dir, "caller-secret"))
	if err != nil {
		return "", fmt.Errorf("caller-secret missing: %w", err)
	}
	if !ValidSecret(key) {
		return "", errors.New("caller-secret invalid (needs >=32 chars of [A-Za-z0-9_-])")
	}
	return key, nil
}

// InternalKey 返回内部 hop 使用的 bearer token。
// Go 版里内部 hop 已是进程内调用，此 key 仅供 /health 探针等
// 向后兼容场景使用，因此缺失时不阻塞启动。
func (s *State) InternalKey() string {
	key, err := readSecretFile(filepath.Join(s.Dir, "internal-secret"))
	if err != nil {
		return ""
	}
	return key
}

// ValidSecret 校验 secret 形状（与 Node 版一致）。
func ValidSecret(value string) bool {
	if len(value) < 32 {
		return false
	}
	for _, c := range value {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// GenerateSecret 生成一个 URL 安全的随机 secret。
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// EnsureSecrets 保证两个 secret 存在；install 子命令与新状态目录初始化用。
func (s *State) EnsureSecrets() error {
	for _, name := range []string{"caller-secret", "internal-secret"} {
		path := filepath.Join(s.Dir, name)
		if existing, err := readSecretFile(path); err == nil && ValidSecret(existing) {
			continue
		}
		secret, err := GenerateSecret()
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}

// ProviderSelection 读写 enabled-providers.json。
type selectionFile struct {
	Providers []string `json:"providers"`
}

// EnabledProviders 返回当前选中的 provider id 列表。
func (s *State) EnabledProviders() []string {
	raw, err := os.ReadFile(filepath.Join(s.Dir, "enabled-providers.json"))
	if err != nil {
		return nil
	}
	var sf selectionFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil
	}
	return sf.Providers
}

// SetEnabledProviders 写回 provider 选择。
func (s *State) SetEnabledProviders(ids []string) error {
	raw, err := json.MarshalIndent(selectionFile{Providers: ids}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir, "enabled-providers.json"), append(raw, '\n'), 0o600)
}

// ProviderEnabled 报告 id（或其家族主 id）是否被选中。
// 协议变体与其主 provider 一起启停，从不单独选择。
func (s *State) ProviderEnabled(id string, canonical func(string) string) bool {
	enabled := s.EnabledProviders()
	if len(enabled) == 0 {
		return false
	}
	for _, candidate := range []string{id, canonical(id)} {
		for _, e := range enabled {
			if e == candidate {
				return true
			}
		}
	}
	return false
}

// CredentialFilePath 返回 provider 凭据文件路径（.secret，0600）。
func (s *State) CredentialFilePath(file string) string {
	return filepath.Join(s.Dir, file)
}

// ReadCredentialFile 读取凭据文件内容（去除首尾空白）。
func (s *State) ReadCredentialFile(file string) (string, bool) {
	value, err := readSecretFile(s.CredentialFilePath(file))
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

// WriteCredentialFile 写入凭据文件，权限 0600。
func (s *State) WriteCredentialFile(file, value string) error {
	path := s.CredentialFilePath(file)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(value)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
