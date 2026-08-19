package main

import (
	"slices"
	"testing"

	"github.com/loyd/codex-router/internal/state"
)

// 托盘开关协议的完整路径（含 dispatch 形状与 --targets 位置参数）：
// set on/off 重写 enabled-providers.json，往返后选择精确反映在落盘内容。
// litellm 是新 provider（注册表有、初始选择无），正好覆盖"追加"分支；
// zai-coding 初始选择已有，覆盖"移除"分支。
func TestControlSetProviderToggle(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnabledProviders([]string{"zai-coding", "opencode-go"}); err != nil {
		t.Fatal(err)
	}

	// litellm on（tray 原始形状：尾部带 --targets codex）。
	if err := cmdControl([]string{"set", "litellm", "on", "--targets", "codex", "--state", dir}); err != nil {
		t.Fatalf("set litellm on: %v", err)
	}
	if got := st.EnabledProviders(); !slices.Contains(got, "litellm") {
		t.Fatalf("litellm must be enabled after set on, got %v", got)
	}

	// litellm off。
	if err := cmdControl([]string{"set", "litellm", "off", "--targets", "codex", "--state", dir}); err != nil {
		t.Fatalf("set litellm off: %v", err)
	}
	if got := st.EnabledProviders(); slices.Contains(got, "litellm") {
		t.Fatalf("litellm must be disabled after set off, got %v", got)
	}
	if !slices.Contains(st.EnabledProviders(), "zai-coding") {
		t.Fatal("toggling litellm must not disturb other providers")
	}

	// 既有 provider 关闭再打开（移除分支）。
	if err := cmdControl([]string{"set", "zai-coding", "off", "--state", dir}); err != nil {
		t.Fatalf("set zai-coding off: %v", err)
	}
	if got := st.EnabledProviders(); slices.Contains(got, "zai-coding") {
		t.Fatalf("zai-coding must be disabled after set off, got %v", got)
	}
	if err := cmdControl([]string{"set", "zai-coding", "on", "--state", dir}); err != nil {
		t.Fatalf("set zai-coding on: %v", err)
	}
	if got := st.EnabledProviders(); !slices.Contains(got, "zai-coding") {
		t.Fatalf("zai-coding must be enabled after set on, got %v", got)
	}
}

// 未知 provider 必须报错（tray 会回滚 UI 开关态）。
func TestControlSetProviderUnknown(t *testing.T) {
	dir := t.TempDir()
	if err := cmdControl([]string{"set", "no-such-provider", "on", "--state", dir}); err == nil {
		t.Fatal("unknown provider must error")
	}
}

// 幂等：重复 on / 重复 off 不产生重复项、不报错。
func TestControlSetProviderIdempotent(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		if err := cmdControl([]string{"set", "litellm", "on", "--state", dir}); err != nil {
			t.Fatalf("set litellm on: %v", err)
		}
	}
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := st.EnabledProviders()
	dup := 0
	for _, id := range got {
		if id == "litellm" {
			dup++
		}
	}
	if dup != 1 {
		t.Fatalf("repeated on must not duplicate, got %v", got)
	}
	for range 2 {
		if err := cmdControl([]string{"set", "litellm", "off", "--state", dir}); err != nil {
			t.Fatalf("set litellm off: %v", err)
		}
	}
	if got := st.EnabledProviders(); len(got) != 0 {
		t.Fatalf("repeated off must leave empty selection, got %v", got)
	}
}
