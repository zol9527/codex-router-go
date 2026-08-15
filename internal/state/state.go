// Package state 管理 ~/.codex-router 状态目录：两个 secret、
// provider 选择、凭据文件。目录格式与原 Node 栈完全一致；从原版
// 嵌在 Codex 家目录里的 ~/.codex/codex-router 迁出（见
// migrateLegacyState），旧目录原样保留、永不删除。
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
	"sync/atomic"

	"github.com/loyd/codex-router/internal/tomlconf"
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
	return filepath.Join(home, ".codex-router")
}

// defaultDirNoEnv 是不带环境覆盖的默认目录；Open 用它判断"这次解析
// 到的是不是新默认位置"，只有是才考虑旧目录迁移。
func defaultDirNoEnv() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex-router")
}

// legacyDir 是原版（Node 栈）与 Go 切换期共用的旧状态目录。
func legacyDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "codex-router")
}

// Open 确认状态目录可用并返回 State 句柄。
func Open(dir string) (*State, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if dir == defaultDirNoEnv() {
		migrateLegacyState(dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &State{Dir: dir}, nil
}

// migrateLegacyState 把旧状态目录一次性复制到新默认目录。
//
// 约束（每条都有明确的失败场景在背后）：
//   - 只在新目录尚不存在时执行 —— 已在的新目录是操作者自己的状态，
//     任何自动写入都可能覆盖人家改过的东西；
//   - 只复制、永不改动旧目录 —— 旧目录同时属于原 Node 栈，删除它
//     违反"不动旧状态"的边界；
//   - caller-secret 随迁，config.toml 里已发布的 base URL（内嵌该
//     key）保持有效，无需重发布；
//   - 每个文件经临时名 + rename 落盘，并发进程（service 与 control
//     同时首跑）最多重复写一遍相同内容，不会读到半截 secret。
func migrateLegacyState(newDir string) {
	legacy := legacyDir()
	if legacy == "" || legacy == newDir {
		return
	}
	if _, err := os.Stat(newDir); err == nil {
		return // 新目录已在：不是首次，操作者状态优先
	}
	entries, err := os.ReadDir(legacy)
	if err != nil || len(entries) == 0 {
		return // 没有旧状态：全新安装
	}
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return
	}
	migrated := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue // 状态目录是平的；子目录不属于状态契约
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(legacy, entry.Name()))
		if err != nil {
			continue
		}
		tmp := filepath.Join(newDir, "."+entry.Name()+".migrating")
		if err := os.WriteFile(tmp, raw, info.Mode().Perm()); err != nil {
			continue
		}
		if err := os.Rename(tmp, filepath.Join(newDir, entry.Name())); err != nil {
			os.Remove(tmp)
			continue
		}
		migrated++
	}
	if migrated > 0 {
		fmt.Fprintf(os.Stderr, "[codex-router] migrated %d state files from %s to %s (original left untouched)\n",
			migrated, legacy, newDir)
	}
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

// ConfigPath 是操作者的凭证配置文件（config.toml）路径。
func (s *State) ConfigPath() string {
	return filepath.Join(s.Dir, "config.toml")
}

// ReadConfigCredential 从 config.toml 读 [table].api_key。
// 文件缺失或不含该表都返回无；解析失败同样返回无 —— 凭证解析在请求
// 路径上，fail-closed 的"无凭证"比带病猜测安全（坏文件的可见性由
// doctor/control 负责，那里会给出带行号的错误）。
// {VAR} 引用先查进程环境，再查 config.toml [env].file 指向的环境文件
// （GUI App 拉起的服务没有登录 shell 的环境，需要这条兜底路径）。
func (s *State) ReadConfigCredential(table string) (string, bool) {
	raw, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		return "", false
	}
	doc, err := tomlconf.Parse(string(raw))
	if err != nil {
		return "", false
	}
	value, ok := doc.Get(table, "api_key")
	if !ok {
		return "", false
	}
	fallback := s.envFileFallback(doc)
	// {VAR} 引用在读取时展开 —— 换环境不改文件，改文件不重启。
	// 引用了未设置的变量时，除了返回"未配置"，还要把悬空的变量名
	// 指出来（providers/doctor 报 "no-key" 却不说为什么，排查要翻
	// 文件；这里记录最后一次悬空引用供诊断面读取）。
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		if name := tomlconf.EnvRefName(value); name != "" && tomlconf.LookupEnvWith(name, fallback) == "" {
			lastDanglingEnvRef.Store([2]string{table, name})
		}
	}
	value = tomlconf.ExpandEnvWith(value, fallback)
	if strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

// envFileFallback 读取 config.toml [env].file 指向的 dotenv 环境文件。
// 未配置、文件缺失或没有可认识的行都返回 nil —— 兜底只是增强，
// 绝不能让主解析路径失败。支持 ~ 开头路径。
func (s *State) envFileFallback(doc *tomlconf.Document) map[string]string {
	path, ok := doc.Get("env", "file")
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		path = filepath.Join(os.Getenv("HOME"), strings.TrimPrefix(path, "~"))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return tomlconf.ParseEnvFile(string(raw))
}

// lastDanglingEnvRef 记录最近一次悬空的 {VAR} 引用（provider, var）。
var lastDanglingEnvRef atomic.Value

// DanglingEnvRef 返回最近的悬空引用（没有则空）。
func DanglingEnvRef() (provider, name string) {
	if v, ok := lastDanglingEnvRef.Load().([2]string); ok {
		return v[0], v[1]
	}
	return "", ""
}

// WriteConfigCredential 原子改写 config.toml 的 [table].api_key，
// 保留操作者手写的注释与其他表。先整文解析校验 —— 解析不了的文件
// 绝不被改写（fail-closed），然后做行级手术。
func (s *State) WriteConfigCredential(table, value string) error {
	return s.rewriteConfig(func(source string) (string, error) {
		return tomlconf.UpsertKey(source, table, "api_key", value)
	})
}

// RemoveConfigTable 删除 config.toml 里的一个表（其他内容原样）。
func (s *State) RemoveConfigTable(table string) error {
	return s.rewriteConfig(func(source string) (string, error) {
		return tomlconf.RemoveTable(source, table)
	})
}

// ConfigParseError 返回 config.toml 的解析错误（无错返回 nil）。
// doctor 用它把坏文件指给操作者。
func (s *State) ConfigParseError() error {
	raw, err := os.ReadFile(s.ConfigPath())
	if err != nil {
		return nil // 文件不存在不是错误
	}
	_, err = tomlconf.Parse(string(raw))
	return err
}

func (s *State) rewriteConfig(transform func(string) (string, error)) error {
	source := ""
	if raw, err := os.ReadFile(s.ConfigPath()); err == nil {
		source = string(raw)
	} else if !os.IsNotExist(err) {
		return err
	}
	next, err := transform(source)
	if err != nil {
		return err
	}
	tmp := s.ConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(next), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.ConfigPath())
}
