package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/discover"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

// providerBaseURL 与 server.(*Server).providerBaseURL 同规则：
// env 覆盖 > config.toml 的 provider 表 base_url > 注册表默认。
// config 档服务自托管 provider（如 LiteLLM）：注册表不内嵌部署地址。
func providerBaseURL(st *state.State, p *registry.Provider) string {
	if p.BaseURLEnv != "" {
		if v := os.Getenv(p.BaseURLEnv); v != "" {
			return v
		}
	}
	family := p.ID
	if p.VariantOf != "" {
		family = p.VariantOf
	}
	if v := strings.TrimSpace(st.ReadConfigBaseURL(family)); v != "" {
		return v
	}
	return p.BaseURL
}

// cmdDiscover：只读的模型发现 —— 实时请求 provider 的 /v1/models，
// 打印全量列表并对照注册表（内嵌 + 覆盖层）分类。
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
	reg, err := registry.LoadWithOverlay(st.Dir, "")
	if err != nil {
		return err
	}
	providerID := reg.CanonicalProviderID(rest[0])
	p := reg.Providers[providerID]
	if p == nil {
		return fmt.Errorf("unknown provider %q (known: zai-coding, opencode-go, litellm)", rest[0])
	}

	resolver := cred.New(st)
	credential, source := resolver.Resolve(p)
	if credential == "" {
		return fmt.Errorf("no credential for %s — set api_key in %s first", p.ID, st.ConfigPath())
	}
	baseURL := providerBaseURL(st, p)
	if baseURL == "" {
		return fmt.Errorf("no base URL for %s — set base_url in %s or export %s", p.ID, st.ConfigPath(), p.BaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	models, err := discover.FetchModels(ctx, &http.Client{Timeout: 20 * time.Second}, baseURL, credential)
	if err != nil {
		return fmt.Errorf("fetch %s/models: %w", baseURL, err)
	}

	known := map[string]bool{}
	registryCount := 0
	for _, m := range reg.Models {
		if reg.CanonicalProviderID(m.Provider) != providerID {
			continue
		}
		registryCount++
		known[discover.NormalizeID(m.UpstreamModel)] = true
		known[discover.NormalizeID(m.GatewayModel)] = true
		known[discover.NormalizeID(m.Slug)] = true
	}
	knownCount := 0
	for _, m := range models {
		if known[discover.NormalizeID(m.ID)] {
			knownCount++
		}
	}

	fmt.Printf("provider %s (%s, credential: %s)\n", p.ID, baseURL, source)
	fmt.Printf("upstream reports %d models; %d already routed\n\n", len(models), knownCount)
	for _, m := range models {
		if known[discover.NormalizeID(m.ID)] {
			fmt.Printf("  = %s\n", m.ID)
			continue
		}
		// 自报元数据随行走：货架声明的能力是部署级真值，注册时自动采信。
		hint := ""
		if m.MaxInputTokens > 0 {
			hint = fmt.Sprintf("   [self-report %d in", int64(m.MaxInputTokens))
			if m.MaxOutputTokens > 0 {
				hint += fmt.Sprintf(" / %d out", int64(m.MaxOutputTokens))
			}
			hint += "]"
		}
		fmt.Printf("  + %s%s   (not registered — control models add %s %q)\n", m.ID, hint, p.ID, m.ID)
	}
	fmt.Println("\n= already routed   + available to register")
	return nil
}
