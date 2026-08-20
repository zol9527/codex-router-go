package state

import (
	"os"
	"path/filepath"
	"testing"
)

// 子代理三模式状态机。语义红线：本地只能收窄证明集合（disabled 降
// v1），永远不能给未证明的模型制造 v2 —— 那属于仓库注册表。
func TestSubagentSettingsDefaults(t *testing.T) {
	dir := t.TempDir()
	settings := ReadSubagentSettings(dir)
	if settings.Mode != SubagentModeProven || settings.Version != 2 {
		t.Fatalf("defaults = %+v, want proven/v2", settings)
	}
}

func TestSubagentLegacyAllSwitchMigrates(t *testing.T) {
	dir := t.TempDir()
	// 第一版的 bool 全局开关。
	os.WriteFile(filepath.Join(dir, "multi-agent-all.json"),
		[]byte(`{"version":1,"enabled":true}`), 0o600)
	if settings := ReadSubagentSettings(dir); settings.Mode != SubagentModeAll {
		t.Fatalf("legacy on = %q, want all", settings.Mode)
	}
	// 损坏的旧文件回落保守默认。
	os.WriteFile(filepath.Join(dir, "multi-agent-all.json"), []byte("not json"), 0o600)
	os.Remove(filepath.Join(dir, "multi-agent-settings.json"))
	if settings := ReadSubagentSettings(dir); settings.Mode != SubagentModeProven {
		t.Fatalf("corrupt legacy = %q, want proven", settings.Mode)
	}
}

func TestSubagentModeAllClearsDisabled(t *testing.T) {
	dir := t.TempDir()
	if _, err := SetSubagentModels(dir, []string{"zai-coding/glm-5.3"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := SetSubagentMode(dir, SubagentModeAll); err != nil {
		t.Fatal(err)
	}
	settings := ReadSubagentSettings(dir)
	if len(settings.Disabled) != 0 {
		t.Errorf("all must clear disabled, got %v", settings.Disabled)
	}
}

func TestSubagentSetModelsEnablesSwitchesToSelected(t *testing.T) {
	dir := t.TempDir()
	settings, err := SetSubagentModels(dir, []string{"zai-coding/glm-5.3", "zai-coding/glm-5.3", ""}, true)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Mode != SubagentModeSelected {
		t.Fatalf("enable from proven = %q, want selected", settings.Mode)
	}
	if len(settings.Enabled) != 1 || settings.Enabled[0] != "zai-coding/glm-5.3" {
		t.Errorf("enabled = %v", settings.Enabled)
	}
	// 关掉：从 enabled 移除、进 disabled。
	settings, err = SetSubagentModels(dir, []string{"zai-coding/glm-5.3"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Enabled) != 0 || len(settings.Disabled) != 1 {
		t.Errorf("after off = %+v", settings)
	}
}

func TestSubagentSettingsRoundTripAndPerms(t *testing.T) {
	dir := t.TempDir()
	if _, err := SetSubagentMode(dir, SubagentModeSelected); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "multi-agent-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("settings file mode = %v, want 0600", info.Mode().Perm())
	}
	if settings := ReadSubagentSettings(dir); settings.Mode != SubagentModeSelected {
		t.Fatalf("round trip = %+v", settings)
	}
}
