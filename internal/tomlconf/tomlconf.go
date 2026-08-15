// Package tomlconf 读写操作者的凭证配置文件（~/.codex-router/config.toml）。
//
// 这是我们自己拥有的文件，契约刻意收窄到 fail-closed 的一个子集：
//   [table]
//   key = "basic string"
//
// 只认：表头（裸键，无点号）、basic string 值（含 \" \\ \n \t \r \uXXXX
// 转义）、# 注释、空白行。dotted key、多行字符串、数组、数字/布尔值、
// 重复表头、重复赋值一律拒绝并指名行号 —— 解析不了的文件绝不被
// "尽力" 猜测（凭证读错比读不到严重）。
//
// 语义参考仓库里 Node 时代的 src/toml-structure.mjs（同为 fail-closed
// 结构 lexer），但那是别人文档的通用解析；这里只服务我们写出的形状。
package tomlconf

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Document 是解析结果：表名 → 键 → 值。保留插入序（写入方要稳定输出）。
type Document struct {
	// order 记录表的出现顺序；tables 承载内容。
	order  []string
	tables map[string]map[string]string
	// keys 内部记每个表的键序。
	keyOrder map[string][]string
}

func newDocument() *Document {
	return &Document{
		tables:   map[string]map[string]string{},
		keyOrder: map[string][]string{},
	}
}

// Tables 按出现顺序返回全部表名。
func (d *Document) Tables() []string {
	out := make([]string, len(d.order))
	copy(out, d.order)
	return out
}

// Table 返回一个表的键值（值拷贝）。不存在返回 nil。
func (d *Document) Table(name string) map[string]string {
	t := d.tables[name]
	if t == nil {
		return nil
	}
	out := make(map[string]string, len(t))
	for k, v := range t {
		out[k] = v
	}
	return out
}

// Get 返回 table.key 的值；ok 报告是否存在。
func (d *Document) Get(table, key string) (string, bool) {
	t := d.tables[table]
	if t == nil {
		return "", false
	}
	v, ok := t[key]
	return v, ok
}

// Set 设置 table.key 并保持表/键的插入序；用于改写已有文档。
func (d *Document) Set(table, key, value string) {
	if d.tables[table] == nil {
		d.tables[table] = map[string]string{}
		d.order = append(d.order, table)
	}
	if _, exists := d.tables[table][key]; !exists {
		d.keyOrder[table] = append(d.keyOrder[table], key)
	}
	d.tables[table][key] = value
}

// RemoveTable 删除一个表（连键序）。不存在是空操作。
func (d *Document) RemoveTable(table string) {
	if d.tables[table] == nil {
		return
	}
	delete(d.tables, table)
	delete(d.keyOrder, table)
	for i, name := range d.order {
		if name == table {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}

// Render 按插入序输出 TOML 文本。只产出解析器自己认的形状 ——
// 读写同一套转义，round-trip 永不产生自己读不回的文件。
func (d *Document) Render() string {
	var sb strings.Builder
	for _, table := range d.order {
		fmt.Fprintf(&sb, "[%s]\n", table)
		for _, key := range d.keyOrder[table] {
			fmt.Fprintf(&sb, "%s = %q\n", key, d.tables[table][key])
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Parse 解析文档。任何不认识的形状返回带行号的错误。
func Parse(source string) (*Document, error) {
	doc := newDocument()
	current := ""
	seen := map[string]bool{} // 表头查重
	for i, raw := range strings.Split(source, "\n") {
		lineNo := i + 1
		line := stripComment(raw)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			name, err := parseHeader(trimmed, lineNo)
			if err != nil {
				return nil, err
			}
			if seen[name] {
				return nil, fmt.Errorf("line %d: duplicate table [%s]", lineNo, name)
			}
			seen[name] = true
			current = name
			doc.tables[current] = map[string]string{}
			doc.order = append(doc.order, current)
			continue
		}
		key, value, err := parseAssignment(trimmed, lineNo)
		if err != nil {
			return nil, err
		}
		if current == "" {
			return nil, fmt.Errorf("line %d: assignment %q before any [table]", lineNo, key)
		}
		if _, dup := doc.tables[current][key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q in [%s]", lineNo, key, current)
		}
		doc.tables[current][key] = value
		doc.keyOrder[current] = append(doc.keyOrder[current], key)
	}
	return doc, nil
}

// stripComment 去掉行内 # 注释 —— 只在引号外生效（值里的 # 不是注释）。
func stripComment(line string) string {
	inString := false
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inString {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if r == '#' && !inString {
			return line[:i]
		}
	}
	return line
}

// parseHeader 解析 [table]。裸键（字母数字-_），无点号无引号。
func parseHeader(trimmed string, lineNo int) (string, error) {
	if !strings.HasSuffix(trimmed, "]") {
		return "", fmt.Errorf("line %d: unterminated table header", lineNo)
	}
	name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	if name == "" {
		return "", fmt.Errorf("line %d: empty table name", lineNo)
	}
	if strings.ContainsAny(name, ".\"'[] \t") {
		return "", fmt.Errorf("line %d: dotted or quoted table names are not supported: [%s]", lineNo, name)
	}
	return name, nil
}

// parseAssignment 解析 key = "value"。
func parseAssignment(trimmed string, lineNo int) (string, string, error) {
	eq := strings.Index(trimmed, "=")
	if eq < 0 {
		return "", "", fmt.Errorf("line %d: not a table header or assignment: %q", lineNo, trimmed)
	}
	key := strings.TrimSpace(trimmed[:eq])
	if key == "" || strings.ContainsAny(key, ".\"'[] \t") {
		return "", "", fmt.Errorf("line %d: unsupported key %q", lineNo, key)
	}
	value := strings.TrimSpace(trimmed[eq+1:])
	if !strings.HasPrefix(value, "\"") || !strings.HasSuffix(value, "\"") || len(value) < 2 {
		return "", "", fmt.Errorf("line %d: value for %q must be a basic string", lineNo, key)
	}
	decoded, err := decodeBasicString(value[1:len(value)-1], lineNo)
	if err != nil {
		return "", "", err
	}
	return key, decoded, nil
}

// decodeBasicString 解码 basic string 内部（已去引号）。只认 Go %q 会
// 产出的转义子集 + \uXXXX —— 与 Render 的写出严格互逆。
func decodeBasicString(s string, lineNo int) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			sb.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("line %d: dangling escape", lineNo)
		}
		switch s[i] {
		case '"', '\\':
			sb.WriteByte(s[i])
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case 'u':
			if i+4 >= len(s) {
				return "", fmt.Errorf("line %d: short \\u escape", lineNo)
			}
			code, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
			if err != nil {
				return "", fmt.Errorf("line %d: bad \\u escape", lineNo)
			}
			sb.WriteRune(rune(code))
			i += 4
		default:
			return "", fmt.Errorf("line %d: unsupported escape \\%c", lineNo, s[i])
		}
	}
	return sb.String(), nil
}

// ---- 行级手术：改写操作者的手写文件 ----
//
// Document.Render 会丢掉注释，而 config.toml 是操作者手写的家 ——
// 注释是他们自己的说明，改一个 key 不许销毁它们。所以写路径是：
// 先 Parse 整文校验（fail-closed：解析不了的文件绝不被碰），
// 再对文本做行级手术，未命中的行原样保留。

// UpsertKey 设置 [table].key。表存在则在其节内替换/追加该键行；
// 表不存在则在文末新建。返回改写后的完整文本。
func UpsertKey(source, table, key, value string) (string, error) {
	if _, err := Parse(source); err != nil {
		return "", err
	}
	lines := strings.Split(source, "\n")
	entry := fmt.Sprintf("%s = %q", key, value)
	start, end := tableSpan(lines, table)
	if start < 0 {
		// 文末新建表；保证与上文之间恰有一个空行分隔。
		out := strings.TrimRight(source, "\n")
		if out != "" {
			out += "\n"
		}
		return out + fmt.Sprintf("[%s]\n%s\n", table, entry), nil
	}
	// 在表节内找已有键行（含被注释掉的同名键也算命中替换位置 ——
	// 模板里的 "# api_key = ..." 正是等着被启用的）。
	commented := "# " + key + " "
	for i := start + 1; i < end; i++ {
		trimmed := strings.TrimSpace(stripComment(lines[i]))
		if strings.HasPrefix(trimmed, key+"=") || strings.HasPrefix(trimmed, key+" ") ||
			strings.HasPrefix(strings.TrimSpace(lines[i]), commented) {
			lines[i] = entry
			return strings.Join(lines, "\n"), nil
		}
	}
	// 节内没有该键：插在表头后一行。
	inserted := append([]string{}, lines[:start+1]...)
	inserted = append(inserted, entry)
	inserted = append(inserted, lines[start+1:]...)
	return strings.Join(inserted, "\n"), nil
}

// RemoveTable 删除 [table] 整节（表头到下一个表头/EOF）。
func RemoveTable(source, table string) (string, error) {
	if _, err := Parse(source); err != nil {
		return "", err
	}
	lines := strings.Split(source, "\n")
	start, end := tableSpan(lines, table)
	if start < 0 {
		return source, nil
	}
	kept := append([]string{}, lines[:start]...)
	kept = append(kept, lines[end:]...)
	out := strings.Join(kept, "\n")
	// 删掉节后收敛多余空行（最多保留一个连续空行段）。
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(out, "\n") + "\n", nil
}

// tableSpan 找 [table] 节的行区间 [start, end)（end = 下一表头或 EOF）。
// 不存在返回 (-1, -1)。表头匹配同样经 stripComment，容忍行内注释。
func tableSpan(lines []string, table string) (int, int) {
	header := "[" + table + "]"
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(stripComment(line))
		if trimmed == header {
			start = i
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(stripComment(lines[i]))
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			end = i
			break
		}
	}
	return start, end
}

// ExpandEnv 把值里的 {NAME} 引用替换为环境变量。解析时不动、解析后
// 展开 —— 同一文件在不同环境下给出不同凭证，且永远不需要重启。
// 未设置的环境变量展开为空串（配置引用了不存在的变量 = 未配置，
// doctor 的 missing 报告会指出来）；不认识的形状（{a-b}、{}、{{}}）
// 原样保留 —— 它们可能就是字面量的一部分。
func ExpandEnv(value string) string {
	var sb strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c != '{' {
			sb.WriteByte(c)
			continue
		}
		end := strings.IndexByte(value[i:], '}')
		if end < 0 {
			sb.WriteByte(c)
			continue
		}
		name := value[i+1 : i+end]
		if !envName(name) {
			sb.WriteByte(c)
			continue
		}
		sb.WriteString(os.Getenv(name))
		i += end
	}
	return sb.String()
}

// envName：环境变量名的形状（字母数字下划线，不以数字开头）。
// 收窄是有意的 —— {some-prose} 是字面量，不是引用。
func envName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
