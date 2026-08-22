package state

import (
	"os"
	"path/filepath"
	"testing"
)

// retryEmptyConfig 写一份最小 config.toml 并返回按它构建的 State。
func retryEmptyConfig(t *testing.T, body string) *State {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &State{Dir: dir}
}

// tomlconf 子集的值一律是带引号字符串：裸 true 会让整个文件解析失败
//（曾实发：写成裸布尔后所有 provider 凭证同时失效）。这里钉住合法
// 形态与容错形态的边界。
func TestReadConfigRetryEmpty(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"quoted true", "[litellm]\nretry_empty_completion = \"true\"\n", true},
		{"quoted false", "[litellm]\nretry_empty_completion = \"false\"\n", false},
		{"missing key", "[litellm]\napi_key = \"x\"\n", false},
		{"garbage value", "[litellm]\nretry_empty_completion = \"yes-please\"\n", false},
		{"bare boolean breaks file", "[litellm]\nretry_empty_completion = true\n", false},
		{"no config file", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := retryEmptyConfig(t, tc.body)
			if tc.body == "" {
				// 空文件场景换成文件缺失（更贴近事故形态：解析失败即静默关）
				if err := os.Remove(filepath.Join(st.Dir, "config.toml")); err != nil {
					t.Fatal(err)
				}
			}
			if got := st.ReadConfigRetryEmpty("litellm"); got != tc.want {
				t.Fatalf("ReadConfigRetryEmpty = %v, want %v", got, tc.want)
			}
		})
	}
}
