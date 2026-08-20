// Package modelmeta 从开源模型数据库 models.dev（opencode 团队维护）
// 查询模型的精确参数 —— 动态注册时用它替代家族克隆的默认值。
//
// 拉取的全库 api.json 缓存在状态目录（24h TTL），离线时用缓存；
// 查不到的模型由调用方回落克隆兜底。
package modelmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	apiURL     = "https://models.dev/api.json"
	cacheFile  = "models-dev.json"
	cacheTTL   = 24 * time.Hour
	fetchLimit = 16 << 20 // 全库 ~4MB，上限 16MB 防异常
)

// Model 是我们关心的字段子集。
type Model struct {
	ID          string
	Name        string
	Description string
	Context     int      // limit.context
	Output      int      // limit.output
	Reasoning   bool     // reasoning
	ToolCall    bool     // tool_call
	InputModes  []string // modalities.input（text/image → 视觉）
}

type apiModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Reasoning   bool   `json:"reasoning"`
	ToolCall    bool   `json:"tool_call"`
	Modalities  struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

type apiDoc map[string]struct {
	Models map[string]apiModel `json:"models"`
}

// Source 提供 URL 覆盖（测试注入 httptest）。
type Source struct {
	BaseURL string // 空 = 官方 api.json
	Client  *http.Client
}

// DefaultSource 是 Lookup 用的默认源；测试可替换 BaseURL 指向桩。
var DefaultSource = Source{}

// Lookup 按数据库 provider ID + 模型 ID 查询。缓存过期则先刷新；
// 刷新失败（离线）回落缓存并继续查。
func Lookup(ctx context.Context, stateDir, providerID, modelID string) (Model, bool, error) {
	doc, err := loadDoc(ctx, stateDir, DefaultSource)
	if err != nil {
		return Model{}, false, err
	}
	entry, ok := doc[providerID].Models[normalizeKey(modelID)]
	if !ok {
		return Model{}, false, nil
	}
	return Model{
		ID:          entry.ID,
		Name:        entry.Name,
		Description: entry.Description,
		Context:     entry.Limit.Context,
		Output:      entry.Limit.Output,
		Reasoning:   entry.Reasoning,
		ToolCall:    entry.ToolCall,
		InputModes:  entry.Modalities.Input,
	}, true, nil
}

func normalizeKey(id string) string {
	// 数据库键与上游模型 ID 的大小写差异很常见（glm-5.3 vs GLM-5.3）。
	return lowerASCII(id)
}

func lowerASCII(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}

func cachePath(stateDir string) string { return filepath.Join(stateDir, cacheFile) }

func loadDoc(ctx context.Context, stateDir string, src Source) (apiDoc, error) {
	path := cachePath(stateDir)
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < cacheTTL {
		if doc, err := readDocFile(path); err == nil {
			return doc, nil
		}
	}
	url := src.BaseURL
	if url == "" {
		url = apiURL
	}
	client := src.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// 离线兜底：过期缓存好过没有。
		if doc, err := readDocFile(path); err == nil {
			return doc, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if doc, err := readDocFile(path); err == nil {
			return doc, nil
		}
		return nil, fmt.Errorf("models.dev HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, fetchLimit))
	if err != nil {
		return nil, err
	}
	var doc apiDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse models.dev: %w", err)
	}
	// 缓存写失败不影响本次结果。
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, path)
	}
	return doc, nil
}

func readDocFile(path string) (apiDoc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc apiDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}
