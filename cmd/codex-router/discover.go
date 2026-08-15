package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/discover"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
)

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
	for _, id := range models {
		if known[discover.NormalizeID(id)] {
			knownCount++
		}
	}

	fmt.Printf("provider %s (%s, credential: %s)\n", p.ID, baseURL, source)
	fmt.Printf("upstream reports %d models; %d already routed\n\n", len(models), knownCount)
	for _, id := range models {
		if known[discover.NormalizeID(id)] {
			fmt.Printf("  = %s\n", id)
		} else {
			fmt.Printf("  + %s   (not registered — control models add %s %q)\n", id, p.ID, id)
		}
	}
	fmt.Println("\n= already routed   + available to register")
	return nil
}
