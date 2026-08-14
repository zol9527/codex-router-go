package main

// 配额卡片命令：account（ChatGPT 原生订阅）与 provider-usage
//（zai/opencode 账号端点 + usage-events 聚合）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/loyd/codex-router/internal/cred"
	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/state"
	"github.com/loyd/codex-router/internal/usage"
)

// controlAccount 读取 ChatGPT 原生订阅的用量窗口（tray 的账号卡片）。
func controlAccount(stateDir string, reg *registry.Registry) error {
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	_ = st
	codexBinary := "codex"
	account, err := usage.ReadCodexAccountUsage(context.Background(), codexBinary)
	payload := map[string]any{}
	if err != nil {
		payload["status"] = "unavailable"
		payload["error"] = err.Error()
	} else {
		payload["status"] = "available"
		payload["usage"] = account
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(raw))
	return nil
}

// controlProviderUsage 聚合 provider 的账号配额与本地用量事件。
func controlProviderUsage(stateDir string, reg *registry.Registry) error {
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	resolver := cred.New(st)
	client := &http.Client{Timeout: 10 * time.Second}

	accounts := []usage.AccountSnapshot{}
	for _, id := range []string{"zai-coding", "opencode-go"} {
		provider := reg.Providers[id]
		if provider == nil {
			continue
		}
		credential, _ := resolver.Resolve(provider)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		snapshot := usage.FetchAccount(ctx, client, id, credential)
		cancel()
		accounts = append(accounts, snapshot)
	}
	payload := map[string]any{
		"accounts":   accounts,
		"localUsage": usage.Aggregate(st.Dir),
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Println(string(raw))
	return nil
}
