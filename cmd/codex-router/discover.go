package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// cmdDiscover：只读的模型发现 —— 实时请求 provider 的 /v1/models，
// 打印**全量**列表并对照注册表分类（已收录 / 新发现）。
// 相比原版 bin/discover-models 的固定清单，这里以上游 API 为准：
// provider 上架了什么就显示什么，注册表只是用来标注"已经有了"。
func cmdDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	stateDir := fs.String("state", state.DefaultDir(), "state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: codex-router discover PROVIDER")
	}

	st, err := state.Open(*stateDir)
	if err != nil {
		return err
	}
	reg, err := registry.LoadDefault("")
	if err != nil {
		return err
	}
	providerID := reg.CanonicalProviderID(rest[0])
	p := reg.Providers[providerID]
	if p == nil {
		return fmt.Errorf("unknown provider %q (known: zai-coding, opencode-go)", rest[0])
	}

	resolver := cred.New(st)
	credential, source := resolver.Resolve(p)
	if credential == "" {
		return fmt.Errorf("no credential for %s — set api_key in %s first", p.ID, st.ConfigPath())
	}
	baseURL := p.BaseURL
	if p.BaseURLEnv != "" {
		if v := os.Getenv(p.BaseURLEnv); v != "" {
			baseURL = v
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	models, err := discoverFetchModels(ctx, http.DefaultClient, baseURL, credential)
	if err != nil {
		return fmt.Errorf("fetch %s/models: %w", baseURL, err)
	}

	known := map[string]bool{}
	for _, m := range reg.Models {
		if reg.CanonicalProviderID(m.Provider) == providerID {
			known[normalizeModelID(m.GatewayModel)] = true
			if m.UpstreamModel != "" {
				known[normalizeModelID(m.UpstreamModel)] = true
			}
		}
	}

	fmt.Printf("provider %s (%s, credential: %s)\n", p.ID, baseURL, source)
	fmt.Printf("upstream reports %d models; registry carries %d of them\n\n",
		len(models), countKnown(models, known))
	for _, id := range models {
		if known[normalizeModelID(id)] {
			fmt.Printf("  = %s\n", id)
		} else {
			fmt.Printf("  + %s   (not in registry)\n", id)
		}
	}
	fmt.Println("\n= already routed   + available to add")
	return nil
}

func countKnown(models []string, known map[string]bool) int {
	count := 0
	for _, id := range models {
		if known[normalizeModelID(id)] {
			count++
		}
	}
	return count
}

// normalizeModelID：上游常见 alias 后缀（:free 之类）与大小写差异
// 不应制造假"新模型"。
func normalizeModelID(id string) string {
	return strings.ToLower(strings.Split(id, ":")[0])
}

// discoverFetchModels 拉取 OpenAI 兼容的 /v1/models 列表。
func discoverFetchModels(ctx context.Context, client *http.Client, baseURL, credential string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("credential rejected (HTTP %d) — check the api_key", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, string(body))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	seen := map[string]bool{}
	for _, m := range payload.Data {
		// 去重按规范化 ID（大小写/alias 后缀），展示保留上游原文。
		key := normalizeModelID(m.ID)
		if m.ID == "" || seen[key] {
			continue
		}
		seen[key] = true
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}
