package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 内嵌注册表是发行形态的唯一来源 —— 它必须与磁盘目录（开发形态）
// 装载出完全相同的 provider/模型集合，否则同一个二进制在开发机与
// App bundle 里表现不同。
func TestEmbeddedRegistryMatchesDisk(t *testing.T) {
	// 测试工作目录是包目录；磁盘注册表在包内 config/（git mv 后的
	// 事实位置），这就是开发形态 Load 的同一棵树。
	disk, err := Load("config")
	if err != nil {
		t.Fatalf("load disk registry: %v", err)
	}
	embedded, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded registry: %v", err)
	}
	if len(disk.Providers) == 0 || len(disk.Models) == 0 {
		t.Fatal("registry unexpectedly empty — test fixture broken")
	}
	if len(disk.Providers) != len(embedded.Providers) {
		t.Errorf("providers: disk %d vs embedded %d", len(disk.Providers), len(embedded.Providers))
	}
	if len(disk.Models) != len(embedded.Models) {
		t.Errorf("models: disk %d vs embedded %d", len(disk.Models), len(embedded.Models))
	}
	for id := range disk.Providers {
		if embedded.Providers[id] == nil {
			t.Errorf("provider %q missing from embedded registry", id)
		}
	}
	for _, m := range disk.Models {
		if embedded.bySlug[m.Slug] == nil {
			t.Errorf("model %q missing from embedded registry", m.Slug)
		}
	}
}

// glm-5.3 上游是 text-only（zai 的视觉能力在 glm-5v-* 变体上），但
// Codex 的贴图入口只认 catalog 里的 image modality —— 声明它只是解锁
// 客户端入口，实际读图由 vision bridge 代办。上游注册表与覆盖层都
// 改不了它（覆盖层只新增不覆盖，内嵌必赢），所以这行声明只能住在
// 内嵌注册表里。拿掉它，Codex 侧就会重新报"此模型不支持图像输入"。
func TestEmbeddedGLM53DeclaresImageInput(t *testing.T) {
	reg, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded registry: %v", err)
	}
	m := reg.ForSlug("zai-coding/glm-5.3")
	if m == nil {
		t.Fatal("zai-coding/glm-5.3 missing from embedded registry")
	}
	for _, modality := range m.InputModalities {
		if strings.EqualFold(modality, "image") {
			return
		}
	}
	t.Errorf("zai-coding/glm-5.3 inputModalities = %v, must declare image (vision bridge backs the picker attach entry)", m.InputModalities)
}

// LoadDefault：显式目录存在则用目录（开发覆盖），不存在落内嵌。
func TestLoadDefaultPrefersExistingDir(t *testing.T) {
	dir := t.TempDir()
	// 空目录可读 —— LoadDefault 应选择它（随后因无 provider 而正常
	// 返回空注册表，而不是落到内嵌）。区分方式：内嵌有 provider。
	reg, err := LoadDefault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Providers) != 0 {
		t.Errorf("empty dir must win over embedded, got %d providers", len(reg.Providers))
	}

	// 不存在的目录 → 内嵌兜底。
	reg, err = LoadDefault(filepath.Join(dir, "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Providers) == 0 {
		t.Error("missing dir must fall back to embedded registry")
	}
	_ = os.RemoveAll(dir)
}
