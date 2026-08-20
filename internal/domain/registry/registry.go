// Package registry 加载并索引 config/ 目录下的 provider 与模型注册表。
// JSON 格式与原 Node 实现完全兼容：config/<vendor>/<vendor>.json 定义
// provider，config/<vendor>/<method>/<model>.json 每文件声明若干模型。
package registry

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Provider 是一个可路由的上游服务定义。
type Provider struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Kind        string `json:"kind"` // 本 fork 只认 "openai-compatible"
	OwnedBy     string `json:"ownedBy"`
	BaseURL     string `json:"baseUrl"`
	BaseURLEnv  string `json:"baseUrlEnv"`
	// DefaultContextWindow 只给动态发现使用：未知模型没有元数据命中时，
	// 用 provider 声明的保守窗口，避免 0 进入 Codex catalog。
	DefaultContextWindow int        `json:"defaultContextWindow,omitempty"`
	Protocol             string     `json:"protocol"` // "" = chat completions; "openai-responses"; "anthropic"
	VariantOf            string     `json:"variantOf"`
	Credential           Credential `json:"credential"`
	// ModelsDevID 是 models.dev 开源库里的 provider ID（动态注册时
	// 查精确参数用），如 "zai-coding-plan" / "opencode-go"。
	ModelsDevID string `json:"modelsDevId,omitempty"`
}

// Credential 描述一个 provider 凭据的三层解析来源。
type Credential struct {
	Environment      []string `json:"environment"`
	File             string   `json:"file"`
	KeychainServices []string `json:"keychainServices"` // 取第一个
	Prompt           string   `json:"prompt"`
}

// ReasoningLevel 是模型声明的一档推理强度。
type ReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// Model 是注册表里的一个可路由模型。
type Model struct {
	Slug            string           `json:"slug"`
	GatewayModel    string           `json:"gatewayModel"`
	UpstreamModel   string           `json:"upstreamModel"`
	Provider        string           `json:"provider"`
	Listed          bool             `json:"listed"`
	DisplayName     string           `json:"displayName"`
	Description     string           `json:"description"`
	Priority        int              `json:"priority"`
	DefaultEffort   string           `json:"defaultEffort"`
	ReasoningLevels []ReasoningLevel `json:"reasoningLevels"`
	ContextWindow   int              `json:"contextWindow"`
	AutoCompact     int              `json:"autoCompact"`
	InputModalities []string         `json:"inputModalities"`
	RequestProfile  string           `json:"requestProfile"`
	CompHash        string           `json:"compHash"`
	// MultiAgentVersion 是 Codex 协作子代理的能力证明标记（"v2" = 可
	// 被 v2 父代理选为分身）。这不是功能开关而是"测试合格章"：只有
	// 真实协作探针（工具调用、密文中继、marker-return spawn、同线程
	// 追问）全部通过的模型才允许在注册表标 v2 —— 声明随仓库发给所有
	// 安装者。本地设置只能收窄（降回 v1），永远不能放大。
	MultiAgentVersion string `json:"multiAgentVersion,omitempty"`
}

// Registry 是加载后的索引视图。
type Registry struct {
	Providers      map[string]*Provider
	Models         []*Model
	bySlug         map[string]*Model
	byGatewayModel map[string]*Model
}

type providerFile struct {
	Version   int        `json:"version"`
	Providers []Provider `json:"providers"`
}

type modelFile struct {
	Version int     `json:"version"`
	Models  []Model `json:"models"`
}

// 注册表内嵌进二进制（go:embed 不允许 ".." 路径，所以目录住在包内）。
// 二进制从此自包含：App bundle 里不需要携带 config/ 目录，注册表变更
// 通过重编二进制分发 —— 本来就是 commit 驱动的。
//
//go:embed all:config
var embeddedConfig embed.FS

// registryFS 是注册表加载的文件源：真目录或内嵌 FS。
type registryFS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
}

// dirFS 把磁盘目录适配成 registryFS。
type dirFS struct{ root string }

func (d dirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(filepath.Join(d.root, name))
}

func (d dirFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.root, name))
}

// Load 读取 configRoot 下的全部注册表片段并构建索引（开发/显式覆盖用）。
func Load(configRoot string) (*Registry, error) {
	return loadFS(dirFS{configRoot})
}

// LoadEmbedded 从内嵌注册表构建索引（发行形态）。
func LoadEmbedded() (*Registry, error) {
	sub, err := fs.Sub(embeddedConfig, "config")
	if err != nil {
		return nil, err
	}
	readDirFS, ok := sub.(registryFS)
	if !ok {
		return nil, fmt.Errorf("embedded registry does not support ReadDir")
	}
	return loadFS(readDirFS)
}

// LoadDefault 是所有命令的入口：显式目录存在就用它（开发、
// --config 覆盖），否则落到内嵌注册表。
func LoadDefault(configRoot string) (*Registry, error) {
	if configRoot != "" {
		if st, err := os.Stat(configRoot); err == nil && st.IsDir() {
			return Load(configRoot)
		}
	}
	return LoadEmbedded()
}

func loadFS(src registryFS) (*Registry, error) {
	r := &Registry{
		Providers:      map[string]*Provider{},
		bySlug:         map[string]*Model{},
		byGatewayModel: map[string]*Model{},
	}
	vendorDirs, err := src.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read config root: %w", err)
	}
	for _, vendor := range vendorDirs {
		if !vendor.IsDir() {
			continue
		}
		vendorDir := vendor.Name()
		definition := path.Join(vendorDir, vendor.Name()+".json")
		if raw, err := src.ReadFile(definition); err == nil {
			var pf providerFile
			if err := json.Unmarshal(raw, &pf); err != nil {
				return nil, fmt.Errorf("parse %s: %w", definition, err)
			}
			for i := range pf.Providers {
				p := pf.Providers[i]
				if p.Kind != "openai-compatible" {
					continue // 本 fork 不认 OAuth / keyless / anonymous provider
				}
				r.Providers[p.ID] = &p
			}
		}
		// 每个 method 子目录里的模型片段
		methodDirs, err := src.ReadDir(vendorDir)
		if err != nil {
			continue
		}
		for _, method := range methodDirs {
			if !method.IsDir() {
				continue
			}
			fragments, err := src.ReadDir(path.Join(vendorDir, method.Name()))
			if err != nil {
				continue
			}
			for _, fragment := range fragments {
				if fragment.IsDir() || !strings.HasSuffix(fragment.Name(), ".json") {
					continue
				}
				rel := path.Join(vendorDir, method.Name(), fragment.Name())
				raw, err := src.ReadFile(rel)
				if err != nil {
					continue
				}
				var mf modelFile
				if err := json.Unmarshal(raw, &mf); err != nil {
					return nil, fmt.Errorf("parse %s: %w", rel, err)
				}
				for _, m := range mf.Models {
					model := m // 拷贝出循环变量
					r.Models = append(r.Models, &model)
					r.bySlug[model.Slug] = &model
					r.byGatewayModel[model.GatewayModel] = &model
				}
			}
		}
	}
	return r, nil
}

// FromDefinitions 用内存中的定义直接构建索引（测试与编程装配用；
// Load 才是磁盘注册表的入口）。
func FromDefinitions(providers []Provider, models []Model) *Registry {
	r := &Registry{
		Providers:      map[string]*Provider{},
		bySlug:         map[string]*Model{},
		byGatewayModel: map[string]*Model{},
	}
	for i := range providers {
		p := providers[i]
		r.Providers[p.ID] = &p
	}
	for _, m := range models {
		model := m
		r.Models = append(r.Models, &model)
		r.bySlug[model.Slug] = &model
		r.byGatewayModel[model.GatewayModel] = &model
	}
	return r
}

// ForSlug 按 picker slug 查模型（Codex 请求里的 model 字段）。
func (r *Registry) ForSlug(slug string) *Model { return r.bySlug[slug] }

// ForGatewayModel 按 gateway id 查模型（翻译层内部使用的 id）。
func (r *Registry) ForGatewayModel(id string) *Model { return r.byGatewayModel[id] }

// ProviderFor 返回模型所属的 provider 定义。
func (r *Registry) ProviderFor(m *Model) *Provider {
	if m == nil {
		return nil
	}
	return r.Providers[m.Provider]
}

// CanonicalProviderID 把协议变体归并到其家族的主 id，
// tray 与 usage 按订阅计费而不是按协议变体。
func (r *Registry) CanonicalProviderID(id string) string {
	if p, ok := r.Providers[id]; ok && p.VariantOf != "" {
		return p.VariantOf
	}
	return id
}

// ResolveBaseURL 解析 provider 的上游地址，规则全仓库唯一一份：
// env 覆盖 > 操作者 config.toml 的 provider 表 base_url（变体归并到
// 家族主项）> 注册表默认。configBaseURL 由调用方注入（server 传
// State 的读取方法，CLI 同理），registry 保持零状态目录依赖；
// 传 nil 跳过 config 档。
func ResolveBaseURL(p *Provider, configBaseURL func(family string) string) string {
	if p.BaseURLEnv != "" {
		if v := os.Getenv(p.BaseURLEnv); v != "" {
			return v
		}
	}
	family := p.ID
	if p.VariantOf != "" {
		family = p.VariantOf
	}
	if configBaseURL != nil {
		if v := strings.TrimSpace(configBaseURL(family)); v != "" {
			return v
		}
	}
	return p.BaseURL
}

// BySlug 按 slug 查模型（未注册返回 nil）。
func (r *Registry) BySlug(slug string) *Model {
	return r.bySlug[slug]
}
