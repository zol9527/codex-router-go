package main

// 配额卡片命令：account（ChatGPT 原生订阅）与 provider-usage
//（zai/opencode 账号端点 + usage-events 聚合）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/loyd/codex-router/internal/app/controlplane"
	"github.com/loyd/codex-router/internal/domain/cred"
	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/state"
	"github.com/loyd/codex-router/internal/domain/usage"
)

// controlAccount 读取 ChatGPT 原生订阅的用量窗口（tray 的账号卡片）。
// controlAccount：tray 把输出直接解码为 CodexAccountUsage（顶层无
// 包装），失败时命令以错误退出 —— tray 在卡片里显示错误文案。
func controlAccount(stateDir string, reg *registry.Registry) error {
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	_ = st
	account, err := usage.ReadCodexAccountUsage(context.Background(), "codex")
	if err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(account, "", "  ")
	fmt.Println(string(raw))
	return nil
}

// controlProviderUsage：本地用量事件按 provider 聚合，再合并账号
// 配额块 —— 形状对齐 tray 的 ProviderUsageSnapshot 解码器。
func controlProviderUsage(stateDir string, reg *registry.Registry) error {
	st, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	resolver := cred.New(st)
	client := &http.Client{Timeout: 10 * time.Second}

	// 原生 openai 流量不经 registry provider，也种子一行 —— 否则本机
	// 最忙的模型直接从表里消失。这里的数字只是路由器观察到的流量；
	// 订阅配额走另一条 Codex account 路径。
	seeds := []usage.ProviderSeed{
		{ID: "openai", DisplayName: "ChatGPT (native)", CredentialType: "oauth"},
	}
	for _, id := range controlplane.ProviderOrder() {
		p := reg.Providers[id]
		if p == nil || p.VariantOf != "" {
			continue
		}
		seeds = append(seeds, usage.ProviderSeed{
			ID: p.ID, DisplayName: p.DisplayName, CredentialType: "api",
		})
	}
	snapshot := usage.BuildProviderUsageSnapshot(st.Dir, seeds, time.Now())
	for i := range snapshot.Providers {
		if snapshot.Providers[i].ID == "openai" {
			snapshot.Providers[i].Account = usage.LocalOnlyAccount()
			continue
		}
		provider := reg.Providers[snapshot.Providers[i].ID]
		if provider == nil {
			snapshot.Providers[i].Account = usage.LocalOnlyAccount()
			continue
		}
		credential, _ := resolver.Resolve(provider)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		acc := usage.FetchAccount(ctx, client, provider.ID, credential)
		cancel()
		snapshot.Providers[i].Account = trayAccount(acc)
	}
	raw, _ := json.MarshalIndent(snapshot, "", "  ")
	fmt.Println(string(raw))
	return nil
}

// trayAccount：AccountSnapshot → tray 的 TrayAccount。错误以 message
// 呈现（tray 的 ProviderAccountUsage 解码的是 message 而非 error）。
func trayAccount(acc usage.AccountSnapshot) usage.TrayAccount {
	out := usage.TrayAccount{
		Status: acc.Status, Source: acc.Source, Metrics: acc.Metrics,
		Plan: acc.Plan, DashboardURL: acc.DashboardURL,
	}
	if acc.Error != "" {
		message := acc.Error
		out.Message = &message
	}
	return out
}
