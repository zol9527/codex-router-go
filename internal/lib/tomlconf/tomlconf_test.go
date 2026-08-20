package tomlconf

import (
	"strings"
	"testing"
)

func TestParseRoundTrip(t *testing.T) {
	source := `# Model Router 凭证配置
[zai-coding]
api_key = "sk-plain"

[opencode-go]
api_key = "with \"quotes\" and \\ backslash"
`
	doc, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := doc.Get("zai-coding", "api_key"); !ok || v != "sk-plain" {
		t.Errorf("zai key = %q ok=%v", v, ok)
	}
	if v, _ := doc.Get("opencode-go", "api_key"); v != `with "quotes" and \ backslash` {
		t.Errorf("escaped value decoded wrong: %q", v)
	}
	// round-trip：Render 的输出必须能被 Parse 读回等价文档。
	doc2, err := Parse(doc.Render())
	if err != nil {
		t.Fatalf("render output no longer parses: %v\n%s", err, doc.Render())
	}
	for _, table := range doc.Tables() {
		for k, v := range doc.Table(table) {
			if v2, _ := doc2.Get(table, k); v2 != v {
				t.Errorf("round-trip diverged: [%s].%s %q != %q", table, k, v2, v)
			}
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"dotted table":     "[a.b]\nkey = \"x\"\n",
		"dotted key":       "[a]\nb.c = \"x\"\n",
		"duplicate table":  "[a]\nk = \"1\"\n[a]\nk = \"2\"\n",
		"duplicate key":    "[a]\nk = \"1\"\nk = \"2\"\n",
		"bare assignment":  "key = \"x\"\n",
		"non-string value": "[a]\nk = 42\n",
		"literal string":   "[a]\nk = 'x'\n",
		"unterminated":     "[a]\nk = \"x\n",
		"empty table name": "[]\nk = \"x\"\n",
		"bad escape":       "[a]\nk = \"\\q\"\n",
	}
	for name, source := range cases {
		if _, err := Parse(source); err == nil {
			t.Errorf("%s: expected rejection, got success", name)
		} else if !strings.Contains(err.Error(), "line ") {
			t.Errorf("%s: error must name the line, got: %v", name, err)
		}
	}
}

func TestCommentHandling(t *testing.T) {
	// 引号内的 # 不是注释；引号外的是。
	doc, err := Parse("[a]\nurl = \"http://x/#frag\" # trailing\n# full line\n")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := doc.Get("a", "url"); v != "http://x/#frag" {
		t.Errorf("in-string # mangled: %q", v)
	}
}

func TestSetRemoveRender(t *testing.T) {
	doc, err := Parse("[zai-coding]\napi_key = \"old\"\n[opencode-go]\napi_key = \"keep\"\n")
	if err != nil {
		t.Fatal(err)
	}
	doc.Set("zai-coding", "api_key", "new")
	doc.RemoveTable("opencode-go")
	out := doc.Render()
	if !strings.Contains(out, "[zai-coding]\napi_key = \"new\"\n") {
		t.Errorf("set did not land:\n%s", out)
	}
	if strings.Contains(out, "opencode-go") {
		t.Errorf("remove table left residue:\n%s", out)
	}
	// 表序稳定：新加的表排在已有表之后。
	doc.Set("third", "k", "v")
	if strings.Index(out, "zai-coding") > strings.Index(doc.Render(), "third") {
		t.Errorf("table order not stable:\n%s", doc.Render())
	}
}

// 手术函数的红线：改一个 key 不许动注释与其他表；解析不了的文件
// 绝不被碰。
func TestUpsertKeyPreservesCommentsAndNeighbors(t *testing.T) {
	source := `# 顶部说明 —— 操作者手写
[zai-coding]
# api_key = "模板注释等着被启用"

[opencode-go]
api_key = "keep-me"
# 下面的注释也要活下来
`
	out, err := UpsertKey(source, "zai-coding", "api_key", "sk-real")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "api_key = \"sk-real\"") {
		t.Errorf("upsert missing:\n%s", out)
	}
	if !strings.Contains(out, "# 顶部说明") || !strings.Contains(out, "# 下面的注释也要活下来") {
		t.Errorf("comments destroyed:\n%s", out)
	}
	if !strings.Contains(out, "api_key = \"keep-me\"") {
		t.Errorf("neighbor table touched:\n%s", out)
	}
	// 结果必须仍然可解析，且读回新值。
	doc, err := Parse(out)
	if err != nil {
		t.Fatalf("upserted file no longer parses: %v\n%s", err, out)
	}
	if v, _ := doc.Get("zai-coding", "api_key"); v != "sk-real" {
		t.Errorf("value not readable after upsert: %q", v)
	}
}

func TestUpsertKeyNewTable(t *testing.T) {
	out, err := UpsertKey("# note\n[zai-coding]\napi_key = \"a\"\n", "opencode-go", "api_key", "b")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[opencode-go]\napi_key = \"b\"") {
		t.Errorf("new table missing:\n%s", out)
	}
	if !strings.Contains(out, "# note") {
		t.Errorf("comment lost:\n%s", out)
	}
}

func TestUpsertKeyRefusesBrokenFile(t *testing.T) {
	broken := "[a]\nk = 42\n" // 非字符串值
	if _, err := UpsertKey(broken, "a", "k", "x"); err == nil {
		t.Error("must refuse to touch a file it cannot parse")
	}
	if _, err := RemoveTable(broken, "a"); err == nil {
		t.Error("RemoveTable must refuse broken files too")
	}
}

func TestRemoveTableLeavesOthers(t *testing.T) {
	source := "# head\n[a]\nk = \"1\"\n\n[b]\nk = \"2\"\n"
	out, err := RemoveTable(source, "a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[a]") || strings.Contains(out, "\"1\"") {
		t.Errorf("table not removed:\n%s", out)
	}
	if !strings.Contains(out, "[b]") || !strings.Contains(out, "# head") {
		t.Errorf("others damaged:\n%s", out)
	}
}

// TestParseEnvFile 钉死 dotenv 兜底解析：export 前缀、成对引号、值里
// 带 =、注释/空行/任意 shell 语法跳过。
func TestParseEnvFile(t *testing.T) {
	got := ParseEnvFile(strings.Join([]string{
		"# comment",
		"",
		"export A_TOKEN=plain",
		"B_TOKEN=\"quoted value\"",
		"C_TOKEN='single'",
		"D_URL=https://x.test/a?b=c=d",
		"garbage line without equals",
		"if [ -f ~/.secrets/env ]; then source it; fi",
		"=no-name",
	}, "\n"))
	want := map[string]string{
		"A_TOKEN": "plain",
		"B_TOKEN": "quoted value",
		"C_TOKEN": "single",
		"D_URL":   "https://x.test/a?b=c=d",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestExpandEnvWithFallbackOrder 钉死展开优先级：进程环境 > 兜底表。
func TestExpandEnvWithFallbackOrder(t *testing.T) {
	t.Setenv("MR_TEST_OVERRIDE", "from-env")
	fb := map[string]string{"MR_TEST_OVERRIDE": "from-file", "MR_TEST_ONLYFILE": "file-value"}
	if got := ExpandEnvWith("{MR_TEST_OVERRIDE}", fb); got != "from-env" {
		t.Errorf("env should win, got %q", got)
	}
	if got := ExpandEnvWith("{MR_TEST_ONLYFILE}", fb); got != "file-value" {
		t.Errorf("fallback missing, got %q", got)
	}
	if got := ExpandEnvWith("{MR_TEST_MISSING}", fb); got != "" {
		t.Errorf("missing should expand empty, got %q", got)
	}
	if got := ExpandEnvWith("literal-{not-a-ref}", fb); got != "literal-{not-a-ref}" {
		t.Errorf("non-ref shape must stay verbatim, got %q", got)
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	// Quote 是仓库唯一一份 TOML 转义：控制字符必须走 \uXXXX
	// （Go %q 的 \x.. 不是合法 TOML），写出的形状 decodeBasicString
	// 必须原样读回。
	values := []string{
		"plain",
		`with "quotes" and \ backslash`,
		"tab\tnewline\ncr\r",
		"ctrl\x01\x02\x7f",
		"unicode 中文 🧩",
		"",
	}
	for _, v := range values {
		quoted := Quote(v)
		doc, err := Parse("[t]\nk = " + quoted + "\n")
		if err != nil {
			t.Errorf("Quote(%q) -> %s 不可解析: %v", v, quoted, err)
			continue
		}
		if got, _ := doc.Get("t", "k"); got != v {
			t.Errorf("round-trip diverged: %q != %q", got, v)
		}
	}
}

func TestParseKeyValue(t *testing.T) {
	cases := []struct {
		line   string
		key    string
		value  string
		wantOK bool
	}{
		{`base_url = "https://x.dev"`, "base_url", "https://x.dev", true},
		{`  key = "v"  # trailing comment`, "key", "v", true},
		{`escaped = "a\"b"`, "escaped", `a"b`, true},
		{`[table]`, "", "", false},
		{`num = 42`, "", "", false},
		{`lit = 'single'`, "", "", false},
		{`broken = "unterminated`, "", "", false},
		{``, "", "", false},
		{`# only comment`, "", "", false},
	}
	for _, c := range cases {
		k, v, ok := ParseKeyValue(c.line)
		if ok != c.wantOK || (ok && (k != c.key || v != c.value)) {
			t.Errorf("ParseKeyValue(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.line, k, v, ok, c.key, c.value, c.wantOK)
		}
	}
}
