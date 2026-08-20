package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 存储模式读取：缺失/损坏回落 always。
func TestReadPresenceMode(t *testing.T) {
	dir := t.TempDir()
	if mode := ReadPresenceMode(dir); mode != PresenceAlways {
		t.Errorf("missing file = %q", mode)
	}
	os.WriteFile(filepath.Join(dir, "presence.json"), []byte(`{corrupt`), 0o600)
	if mode := ReadPresenceMode(dir); mode != PresenceAlways {
		t.Errorf("corrupt file = %q", mode)
	}
	os.WriteFile(filepath.Join(dir, "presence.json"), []byte(`{"version":1,"mode":"follow-codex"}`), 0o600)
	if mode := ReadPresenceMode(dir); mode != PresenceFollowCodex {
		t.Errorf("stored follow = %q", mode)
	}
	// 版本不符回落。
	os.WriteFile(filepath.Join(dir, "presence.json"), []byte(`{"version":2,"mode":"follow-codex"}`), 0o600)
	if mode := ReadPresenceMode(dir); mode != PresenceAlways {
		t.Errorf("wrong version = %q", mode)
	}
}

// 写入往返。
func TestSetPresenceMode(t *testing.T) {
	dir := t.TempDir()
	if err := SetPresenceMode(dir, PresenceFollowCodex); err != nil {
		t.Fatal(err)
	}
	if mode := ReadPresenceMode(dir); mode != PresenceFollowCodex {
		t.Errorf("round trip = %q", mode)
	}
	if err := SetPresenceMode(dir, "bogus"); err == nil {
		t.Error("invalid mode must be rejected")
	}
}

// effective 覆盖：终端 codex 存在时 follow 钉在 always
// （检测宁可误报 —— 假阴性丢请求）。
func TestEffectivePresenceOverride(t *testing.T) {
	dir := t.TempDir()
	SetPresenceMode(dir, PresenceFollowCodex)
	mode := EffectivePresenceMode(dir)
	if TerminalCodexInstalled() && mode != PresenceAlways {
		t.Errorf("terminal codex must pin always, got %q", mode)
	}
	// 存储模式不被改写（覆盖是读取时的，不是持久的）。
	if ReadPresenceMode(dir) != PresenceFollowCodex {
		t.Error("stored mode must not be rewritten by the override")
	}
	// always 模式原样。
	SetPresenceMode(dir, PresenceAlways)
	if EffectivePresenceMode(dir) != PresenceAlways {
		t.Error("always stays always")
	}
	if ServiceFollowsHostApps(dir) {
		t.Error("always mode never follows host apps")
	}
}

// tray 的 RouterPresence 四个字段全部非可选；harnessPublished 随 dsh
// 目标砍掉后仍须发显式 false —— 缺键让 tray 整个快照解码失败。
func TestPresenceSnapshotCarriesAllTrayKeys(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(PresenceSnapshot(dir))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mode", "effectiveMode", "terminalCodex", "harnessPublished"} {
		if _, ok := snapshot[key]; !ok {
			t.Errorf("presence snapshot missing key %q: %s", key, raw)
		}
	}
	if snapshot["harnessPublished"] != false {
		t.Errorf("harnessPublished = %v, want false", snapshot["harnessPublished"])
	}
}
