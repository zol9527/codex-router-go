// Package registry 的用户覆盖层：~/.codex-router/user-models.json。
//
// 覆盖层是动态注册的唯一手写入口（discover/models.dev 同步、control
// models add 都写这里）。合并纪律：
//   - 只新增，不覆盖 —— 与内嵌注册表同名（slug 或 gatewayModel 撞车）
//     的条目以内嵌为准：升级二进制带来的正式收录永远赢过本地快照，
//     行为可预期
//   - 服务端热重载：文件变化后由 SIGUSR1 触发重新合并（见 cmdServe）
package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// UserModelsFile 是覆盖层文件名（位于状态目录）。
const UserModelsFile = "user-models.json"

// UserModelEntry 是覆盖层里的一个模型 + 来源标注（谁加的、什么时候）。
type UserModelEntry struct {
	Model   Model  `json:"model"`
	Source  string `json:"source"` // "modelsdev" | "clone" | "manual"
	AddedAt string `json:"addedAt"`
}

type userModelsFile struct {
	Version int              `json:"version"`
	Models  []UserModelEntry `json:"models"`
}

// UserModelsPath 返回覆盖层文件路径。
func UserModelsPath(stateDir string) string {
	return filepath.Join(stateDir, UserModelsFile)
}

// ReadUserModels 读取覆盖层条目（文件缺失/损坏 → 空列表，绝不阻塞启动）。
func ReadUserModels(stateDir string) []UserModelEntry {
	raw, err := os.ReadFile(UserModelsPath(stateDir))
	if err != nil {
		return nil
	}
	var parsed userModelsFile
	if json.Unmarshal(raw, &parsed) != nil || parsed.Version != 1 {
		return nil
	}
	return parsed.Models
}

// WriteUserModels 原子写覆盖层（0600 —— 模型定义不敏感，但目录纪律
// 统一 0600）。
func WriteUserModels(stateDir string, entries []UserModelEntry) error {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Model.Slug < entries[j].Model.Slug
	})
	raw, err := json.MarshalIndent(userModelsFile{Version: 1, Models: entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp := UserModelsPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, UserModelsPath(stateDir))
}

// ApplyOverlay 把覆盖层合并进注册表副本：撞车（slug 或 gatewayModel
// 与内嵌重复）跳过并计入 skipped —— 内嵌正式收录永远优先。
// 返回新 Registry（原注册表不动）与被跳过的 slug。
func ApplyOverlay(base *Registry, entries []UserModelEntry) (*Registry, []string) {
	merged := &Registry{
		Providers:      base.Providers,
		Models:         append([]*Model{}, base.Models...),
		bySlug:         map[string]*Model{},
		byGatewayModel: map[string]*Model{},
	}
	for _, m := range merged.Models {
		merged.bySlug[m.Slug] = m
		merged.byGatewayModel[m.GatewayModel] = m
	}
	skipped := []string{}
	for _, entry := range entries {
		m := entry.Model
		if m.Slug == "" || merged.Providers[m.Provider] == nil {
			continue // 无 provider 的条目没有意义
		}
		if merged.bySlug[m.Slug] != nil || (m.GatewayModel != "" && merged.byGatewayModel[m.GatewayModel] != nil) {
			skipped = append(skipped, m.Slug)
			continue
		}
		model := m
		merged.Models = append(merged.Models, &model)
		merged.bySlug[m.Slug] = &model
		if model.GatewayModel != "" {
			merged.byGatewayModel[model.GatewayModel] = &model
		}
	}
	return merged, skipped
}

// LoadWithOverlay = 内嵌注册表 + 覆盖层（control/serve 的统一入口）。
func LoadWithOverlay(stateDir, configDirOverride string) (*Registry, error) {
	base, err := LoadDefault(configDirOverride)
	if err != nil {
		return nil, err
	}
	merged, _ := ApplyOverlay(base, ReadUserModels(stateDir))
	return merged, nil
}
