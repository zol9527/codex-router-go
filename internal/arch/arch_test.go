// Package arch 钉住 internal/ 四层目录的车道规则（ADR-0005）：
// 依赖边只允许朝下（或留在层内），防止"顺手 import"把分层悄悄侵蚀
// 回平铺时代的混沌。本包 test-only：用 go/parser 扫源码 import 语句，
// 不 import 任何被测包，自身不参与车道。
package arch

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowed[fromTier] 列出 fromTier 的包允许依赖的层。
// lib 零内部依赖（共享实现唯一份）；domain 只向下到 lib；
// engine 到 lib/domain；app 全部可依赖 —— 只有 cmd 依赖 app。
var allowed = map[string]map[string]bool{
	"lib":    {},
	"domain": {"lib": true, "domain": true},
	"engine": {"lib": true, "domain": true, "engine": true},
	"app":    {"lib": true, "domain": true, "engine": true, "app": true},
}

func TestImportLanes(t *testing.T) {
	root := repoRoot(t)
	internalPrefix := modulePath(t, root) + "/internal/"

	files, edges := 0, 0
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		parts := strings.Split(rel, "/")
		if len(parts) < 3 {
			return nil
		}
		fromTier := parts[1]
		if _, ok := allowed[fromTier]; !ok {
			return nil // arch 等非车道目录不参与
		}
		files++
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("%s: 解析失败: %v", rel, err)
			return nil
		}
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, internalPrefix) {
				continue
			}
			toTier := strings.Split(strings.TrimPrefix(p, internalPrefix), "/")[0]
			edges++
			if !allowed[fromTier][toTier] {
				t.Errorf("车道违规: %s（%s 层）import %s（%s 层）", rel, fromTier, p, toTier)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 防空洞转：扫描必须覆盖真实规模的文件与边；module 路径改名导致
	// 前缀全部失配时，这里会以"边数骤减"的形式暴露。
	if files < 50 {
		t.Errorf("扫描到的非测试源文件只有 %d 个，疑似扫描范围或模块路径失配", files)
	}
	if edges < 30 {
		t.Errorf("扫描到的 internal 依赖边只有 %d 条，疑似模块路径失配", edges)
	}
	t.Logf("扫描 %d 个非测试源文件、%d 条 internal 依赖边，车道全部合法", files, edges)
}

// repoRoot 从测试工作目录（internal/arch）向上找 go.mod。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
}

// modulePath 读 go.mod 首行，车道规则不硬编码模块路径。
func modulePath(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("go.mod 缺少 module 声明")
	return ""
}
