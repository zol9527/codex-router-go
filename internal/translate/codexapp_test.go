package translate

import "testing"

// 快照加载与形态：两个 namespace、codex_app 17 个工具、
// plugin_management 1 个。
func TestCodexAppSnapshotLoads(t *testing.T) {
	if len(appNamespaces) != 2 {
		t.Fatalf("snapshot namespaces = %d, want 2: %v", len(appNamespaces), keysOf(appNamespaces))
	}
	if len(appNamespaces["codex_app"]) != 17 {
		t.Errorf("codex_app tools = %d, want 17", len(appNamespaces["codex_app"]))
	}
	if len(appNamespaces["plugin_management"]) != 1 {
		t.Errorf("plugin_management tools = %d, want 1", len(appNamespaces["plugin_management"]))
	}
	// 合并引用的关键工具在快照里。
	for _, want := range []string{"automation_update", "create_thread", "navigate_to_codex_page"} {
		if appNamespaces["codex_app"][want] == nil {
			t.Errorf("snapshot tool %q missing", want)
		}
	}
}

// 精简 namespace 合并：客户端 3 工具优先，快照补缺。
func TestMergeCodexAppToolsFillsDeferred(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "namespace", "name": "codex_app",
			"tools": []any{
				map[string]any{"name": "create_thread", "description": "client version wins",
					"inputSchema": map[string]any{"type": "object"}},
			},
		},
		map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}},
	}
	merged, changed := MergeCodexAppTools(tools)
	if !changed {
		t.Fatal("merge should happen")
	}
	var namespace map[string]any
	sawShell := false
	for _, raw := range merged {
		tool := raw.(map[string]any)
		if tool["type"] == "namespace" && tool["name"] == "codex_app" {
			namespace = tool
		}
		if tool["name"] == "shell" {
			sawShell = true
		}
	}
	if namespace == nil || !sawShell {
		t.Fatalf("merged list lost entries: %+v", merged)
	}
	children := namespace["tools"].([]any)
	if len(children) != 17 {
		t.Errorf("merged codex_app = %d tools, want 17 (client 1 + deferred 16)", len(children))
	}
	// 客户端定义优先。
	for _, raw := range children {
		fn := raw.(map[string]any)
		if fn["name"] == "create_thread" && fn["description"] != "client version wins" {
			t.Error("client-provided definition must win over snapshot")
		}
	}
	// plugin_management 原本缺失 → 追加完整快照。
	sawPlugin := false
	for _, raw := range merged {
		if tool, ok := raw.(map[string]any); ok && tool["name"] == "plugin_management" {
			sawPlugin = true
		}
	}
	if !sawPlugin {
		t.Error("missing plugin_management namespace must be appended")
	}
}

// namespace 整个缺失：追加完整快照。
func TestMergeCodexAppToolsAppendsMissing(t *testing.T) {
	tools := []any{
		map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}},
	}
	merged, changed := MergeCodexAppTools(tools)
	if !changed {
		t.Fatal("appending missing namespace is a change")
	}
	if len(merged) != 3 { // shell + codex_app + plugin_management
		t.Errorf("merged = %d entries, want 3", len(merged))
	}
}

// 非 namespace 请求（无 tools）：不合并。
func TestMergeCodexAppToolsNoTools(t *testing.T) {
	if _, changed := MergeCodexAppTools(nil); changed {
		t.Error("nil tools must not merge")
	}
}

func keysOf(m map[string]map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
