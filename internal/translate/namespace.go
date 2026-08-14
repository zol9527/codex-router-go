package translate

// namespace 工具拍平，移植自 namespace-relay.mjs + tool-schema-root.mjs +
// tool-arguments.mjs。
//
// Codex 把大部分工具集以 `type: "namespace"` 形态下发（协作运行时、
// app 工具集、全部 MCP server）。chat-completions 上游只认普通 function
// 工具，所以请求方向把 namespace 展开成 `<namespace>__<tool>` 扁平函数、
// 历史同步改名；响应方向把模型发出的扁平调用名还原成客户端派发的
// `{name, namespace}` 形态。还原永远通过请求时构建的精确映射解析，
// 绝不按分隔符拆名 —— namespace 名本身可能含 `__`
//（如 `mcp__codex_apps__github`）。

import (
	"encoding/json"
	"regexp"
	"strings"
)

// NamespaceDelimiter 是扁平名的分隔符。
const NamespaceDelimiter = "__"

// SpawnModelTools 是继承会话模型的本地线程创建工具。
var spawnModelTools = map[string]bool{"create_thread": true}

const codexAppPrefix = "codex_app" + NamespaceDelimiter
const collaborationNS = "collaboration"

// NamespaceIndex 是一次请求的 namespace 工具索引
// （namespaces、还原映射、spawn_agent 模型白名单）。
type NamespaceIndex struct {
	// namespaces: namespace 名 → 其工具名集合。
	Namespaces map[string]map[string]bool
	// flatToNative: 扁平名 → {namespace, name}。
	flatToNative map[string]nativeToolRef
	// bareToNamespaces: 裸工具名 → 拥有它的 namespace 列表（判唯一归属）。
	bareToNamespaces map[string][]string
	// spawnAgentModels: spawn_agent 的 inputSchema 声明过的模型枚举。
	spawnAgentModels map[string]bool
}

type nativeToolRef struct {
	namespace string
	name      string
}

// FlattenResult 是 FlattenNamespaceTools 的返回值。
type FlattenResult struct {
	Tools      any
	Flattened  bool
	Namespaces map[string]map[string]bool
	// spawnAgentModels 是从 spawn_agent 的 inputSchema 提取的模型枚举
	//（响应方向白名单）。
	spawnAgentModels map[string]bool
}

// FlattenNamespaceTools 把 namespace 条目展开成普通 function 工具。
//   - tool_search 控制工具丢弃（namespace 已全部展开，控制冗余且被
//     chat 上游整体拒绝）；
//   - inputSchema 别名为 parameters，并对 provider 副本做 schema 归一
//     （union 根合并、矛盾 literal 丢弃）—— 客户端的 inputSchema 原样保留；
//   - 收集 collaboration.spawn_agent 的模型枚举供响应方向白名单用。
func FlattenNamespaceTools(tools any) FlattenResult {
	toolList, ok := tools.([]any)
	if !ok {
		return FlattenResult{Tools: tools, Namespaces: map[string]map[string]bool{}}
	}
	index := newNamespaceIndex()
	flattened := make([]any, 0, len(toolList))
	changed := false
	for _, raw := range toolList {
		tool, ok := raw.(map[string]any)
		if !ok {
			flattened = append(flattened, raw)
			continue
		}
		toolType, _ := tool["type"].(string)
		if toolType == "tool_search" {
			changed = true
			continue
		}
		if toolType != "namespace" {
			flattened = append(flattened, raw)
			continue
		}
		namespaceName, _ := tool["name"].(string)
		children, _ := tool["tools"].([]any)
		names := map[string]bool{}
		for _, childRaw := range children {
			fn, ok := childRaw.(map[string]any)
			if !ok {
				continue
			}
			childName, _ := fn["name"].(string)
			if childName == "" {
				continue
			}
			next := map[string]any{}
			for k, v := range fn {
				next[k] = v
			}
			// namespace 子工具不带 type；chat 上游的工具形态是普通
			// function（LiteLLM 适配层补的就是这个字段）。
			next["type"] = "function"
			next["name"] = namespaceName + NamespaceDelimiter + childName
			// Codex 命名函数 schema 为 inputSchema；chat 上游只读
			// parameters。没有这层别名，每个 namespace 子工具都会以
			// 空 schema 到达 provider，MCP 调用拿不到参数定义。
			clientSchema := fn["parameters"]
			if clientSchema == nil {
				clientSchema = fn["inputSchema"]
			}
			if clientSchema != nil {
				next["parameters"] = ProviderToolSchema(clientSchema)
			}
			flattened = append(flattened, next)
			names[childName] = true
			if namespaceName == collaborationNS && childName == "spawn_agent" {
				collectSpawnAgentModels(fn, index)
			}
		}
		if len(names) > 0 {
			index.Namespaces[namespaceName] = names
			changed = true
		}
	}
	if !changed {
		return FlattenResult{Tools: tools, Namespaces: index.Namespaces, spawnAgentModels: index.spawnAgentModels}
	}
	index.rebuild()
	return FlattenResult{Tools: flattened, Flattened: true, Namespaces: index.Namespaces, spawnAgentModels: index.spawnAgentModels}
}

func newNamespaceIndex() *NamespaceIndex {
	return &NamespaceIndex{
		Namespaces:       map[string]map[string]bool{},
		flatToNative:     map[string]nativeToolRef{},
		bareToNamespaces: map[string][]string{},
		spawnAgentModels: map[string]bool{},
	}
}

// rebuild 从 Namespaces 重建还原映射。
func (n *NamespaceIndex) rebuild() {
	n.flatToNative = map[string]nativeToolRef{}
	n.bareToNamespaces = map[string][]string{}
	for namespace, names := range n.Namespaces {
		for name := range names {
			n.flatToNative[namespace+NamespaceDelimiter+name] = nativeToolRef{namespace, name}
			if !containsString(n.bareToNamespaces[name], namespace) {
				n.bareToNamespaces[name] = append(n.bareToNamespaces[name], namespace)
			}
		}
	}
}

// Index 从 flatten 结果构建响应方向的索引。
func (r FlattenResult) Index() *NamespaceIndex {
	index := newNamespaceIndex()
	index.Namespaces = r.Namespaces
	if index.Namespaces == nil {
		index.Namespaces = map[string]map[string]bool{}
	}
	index.spawnAgentModels = r.spawnAgentModels
	index.rebuild()
	return index
}

// collectSpawnAgentModels 从 spawn_agent 的 inputSchema.properties.model
// 提取字符串值（const / enum / anyOf/oneOf/allOf 分支递归）。
func collectSpawnAgentModels(fn map[string]any, index *NamespaceIndex) {
	schema, _ := fn["inputSchema"].(map[string]any)
	if schema == nil {
		return
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		return
	}
	modelSchema, _ := properties["model"].(map[string]any)
	if modelSchema == nil {
		return
	}
	for _, value := range schemaStringValues(modelSchema) {
		index.spawnAgentModels[value] = true
	}
}

func schemaStringValues(schema map[string]any) []string {
	if schema == nil {
		return nil
	}
	var values []string
	if c, ok := schema["const"].(string); ok {
		values = append(values, c)
	}
	if enum, ok := schema["enum"].([]any); ok {
		for _, v := range enum {
			if s, ok := v.(string); ok {
				values = append(values, s)
			}
		}
	}
	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		branches, _ := schema[keyword].([]any)
		for _, branch := range branches {
			if m, ok := branch.(map[string]any); ok {
				values = append(values, schemaStringValues(m)...)
			}
		}
	}
	return values
}

// FlattenNamespacedHistory 把历史 function_call 的名字改成扁平形态，
// 让模型的 transcript 与它的工具列表一致——否则模型模仿历史发出裸名，
// 客户端答 "unsupported call"，且每次失败又多一个裸名示例。
// 三种历史形态：已扁平（不动）、{name, namespace}（精确改名）、
// 裸名且唯一归属一个 namespace（改名）。
func FlattenNamespacedHistory(input []any, namespaces map[string]map[string]bool) []any {
	if len(namespaces) == 0 {
		return input
	}
	index := newNamespaceIndex()
	index.Namespaces = namespaces
	index.rebuild()
	out := make([]any, len(input))
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "function_call" {
			out[i] = raw
			continue
		}
		name, _ := item["name"].(string)
		if name == "" {
			out[i] = raw
			continue
		}
		if _, alreadyFlat := index.flatToNative[name]; alreadyFlat {
			out[i] = raw
			continue
		}
		namespaceValue, hasNamespaceField := item["namespace"]
		namespace, _ := namespaceValue.(string)
		if hasNamespaceField && namespace != "" {
			if index.Namespaces[namespace][name] {
				renamed := shallowCopyWithout(item, "namespace")
				renamed["name"] = namespace + NamespaceDelimiter + name
				out[i] = renamed
				continue
			}
			out[i] = raw
			continue
		}
		// 无 namespace 字段的裸名：仅当唯一归属时改。
		if owners := index.bareToNamespaces[name]; len(owners) == 1 {
			renamed := shallowCopyWithout(item, "namespace")
			renamed["name"] = owners[0] + NamespaceDelimiter + name
			out[i] = renamed
			continue
		}
		out[i] = raw
	}
	return out
}

// RewriteFunctionCallItem 把一个 function_call item 还原为客户端原生
// {name, namespace} 形态。精确解析扁平名；裸名仅在唯一归属时还原，
// 冲突保持原样而不是猜测运行时归属。
// 返回 nil 表示无需改写。附带 spawn_agent 的模型白名单清洗、
// create_thread 的会话模型注入、arguments 整数 token 修复。
func (n *NamespaceIndex) RewriteFunctionCallItem(item map[string]any, sessionModel string) map[string]any {
	if n == nil || item == nil || item["type"] != "function_call" {
		return nil
	}
	name, _ := item["name"].(string)
	rewritten := item
	changed := false
	if resolved, ok := n.flatToNative[name]; ok {
		rewritten = shallowCopy(item)
		rewritten["name"] = resolved.name
		rewritten["namespace"] = resolved.namespace
		changed = true
	} else if _, has := item["namespace"]; !has {
		if owners := n.bareToNamespaces[name]; len(owners) == 1 {
			rewritten = shallowCopy(item)
			rewritten["namespace"] = owners[0]
			changed = true
		}
	}
	// spawn_agent 的 model 参数：不在请求 schema 声明的枚举里就删掉，
	// 否则客户端校验拒绝整个调用。
	if itemNS, _ := rewritten["namespace"].(string); itemNS == collaborationNS && rewritten["name"] == "spawn_agent" {
		if cleaned := sanitizeSpawnAgentModelArgs(rewritten, n.spawnAgentModels); cleaned != nil {
			rewritten = cleaned
			changed = true
		}
	}
	// create_thread 无显式模型时注入会话模型（本地线程继承路由会话）。
	if injected := injectSessionModelForSpawnCalls(rewritten, sessionModel); injected != nil {
		rewritten = injected
		changed = true
	}
	// 整数 token 修复：部分模型把整数发成 20000.0，客户端 serde 拒绝。
	if argsText, _ := rewritten["arguments"].(string); argsText != "" {
		if fixed := CoerceFunctionCallArguments(argsText); fixed != argsText {
			rewritten = shallowCopy(rewritten)
			rewritten["arguments"] = fixed
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return rewritten
}

// sanitizeSpawnAgentModelArgs 删掉不在白名单里的 model 参数。
func sanitizeSpawnAgentModelArgs(item map[string]any, allowed map[string]bool) map[string]any {
	if len(allowed) == 0 {
		return nil
	}
	argsText, _ := item["arguments"].(string)
	if argsText == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsText), &args); err != nil || args == nil {
		return nil
	}
	model, ok := args["model"].(string)
	if !ok || allowed[model] {
		return nil
	}
	delete(args, "model")
	next, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	rewritten := shallowCopy(item)
	rewritten["arguments"] = string(next)
	return rewritten
}

// injectSessionModelForSpawnCalls：会话模型注入 create_thread。
// 返回新 map 或 nil（无需注入）。
func injectSessionModelForSpawnCalls(item map[string]any, sessionModel string) map[string]any {
	if sessionModel == "" {
		return nil
	}
	namespace, _ := item["namespace"].(string)
	name, _ := item["name"].(string)
	isSpawn := false
	if strings.HasPrefix(name, codexAppPrefix) {
		isSpawn = spawnModelTools[strings.TrimPrefix(name, codexAppPrefix)]
	} else if namespace == "codex_app" {
		isSpawn = spawnModelTools[name]
	}
	if !isSpawn {
		return nil
	}
	argsText, _ := item["arguments"].(string)
	if argsText == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsText), &args); err != nil || args == nil {
		return nil
	}
	if _, has := args["model"]; has {
		return nil
	}
	// 云端任务要求省略 model，不注入。
	if target, _ := args["target"].(map[string]any); target != nil {
		if target["type"] == "chatgptWorkCloud" {
			return nil
		}
	}
	args["model"] = sessionModel
	next, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	rewritten := shallowCopy(item)
	rewritten["arguments"] = string(next)
	return rewritten
}

// ---- 整数 token 修复（tool-arguments.mjs）----

var jsonNumberPattern = regexp.MustCompile(`-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?`)
var plainIntegerPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

// integerSpelling 从 token 拼写判断整数值。字符串级判定，不用浮点
// （浮点无法表达全部 u64，也不能区分 20000 和 20000.0）。
func integerSpelling(token string) string {
	parts := jsonNumberPattern.FindString(token)
	if parts == "" || parts != token {
		return ""
	}
	// 手工分解 sign / int / frac / exp。
	rest := token
	negative := false
	if strings.HasPrefix(rest, "-") {
		negative = true
		rest = rest[1:]
	}
	var intPart, fracPart string
	exp := 0
	if i := strings.IndexAny(rest, ".eE"); i >= 0 {
		intPart = rest[:i]
		rest = rest[i:]
		if strings.HasPrefix(rest, ".") {
			rest = rest[1:]
			if j := strings.IndexAny(rest, "eE"); j >= 0 {
				fracPart = rest[:j]
				rest = rest[j:]
			} else {
				fracPart = rest
				rest = ""
			}
		}
		if strings.HasPrefix(rest, "e") || strings.HasPrefix(rest, "E") {
			expText := rest[1:]
			expSign := 1
			if strings.HasPrefix(expText, "+") {
				expText = expText[1:]
			} else if strings.HasPrefix(expText, "-") {
				expSign = -1
				expText = expText[1:]
			}
			if expText == "" {
				return ""
			}
			for _, c := range expText {
				if c < '0' || c > '9' {
					return ""
				}
			}
			if len(expText) > 6 {
				return "" // 指数超出合理范围，放弃判定
			}
			for _, c := range expText {
				exp = exp*10 + int(c-'0')
			}
			exp *= expSign
		}
	} else {
		intPart = rest
	}
	digits := intPart + fracPart
	point := len(intPart) + exp
	if point > 40 || point < -40 {
		return ""
	}
	// 小数点后全部为零才是整数。
	for i := point; i < len(digits); i++ {
		if i >= 0 && digits[i] != '0' {
			return ""
		}
	}
	var integerDigits string
	switch {
	case point <= 0:
		integerDigits = "0"
	case point >= len(digits):
		integerDigits = digits + strings.Repeat("0", point-len(digits))
	default:
		if point < 0 {
			return ""
		}
		integerDigits = digits[:point]
	}
	integerDigits = strings.TrimLeft(integerDigits, "0")
	if integerDigits == "" {
		integerDigits = "0"
	}
	if integerDigits == "0" {
		return "0"
	}
	if negative {
		return "-" + integerDigits
	}
	return integerDigits
}

// integerToken 把 "20000.0" / "2e4" 这类整数值 token 改写为整数字面量。
func integerToken(token string) string {
	if plainIntegerPattern.MatchString(token) && token != "-0" {
		return token
	}
	if spelling := integerSpelling(token); spelling != "" {
		return spelling
	}
	return token
}

// CoerceFunctionCallArguments 在原始 JSON 文本上把整数值的浮点 token
// 改写为整数字面量（字符串感知，不进字符串内部），改写后仍须是合法 JSON。
func CoerceFunctionCallArguments(raw string) string {
	if raw == "" {
		return raw
	}
	var out strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(raw); {
		ch := raw[i]
		if inString {
			out.WriteByte(ch)
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			i++
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			i++
			continue
		}
		if ch == '-' || (ch >= '0' && ch <= '9') {
			if match := jsonNumberPattern.FindString(raw[i:]); match != "" {
				out.WriteString(integerToken(match))
				i += len(match)
				continue
			}
		}
		out.WriteByte(ch)
		i++
	}
	rewritten := out.String()
	if rewritten == raw {
		return raw
	}
	var probe any
	if json.Unmarshal([]byte(rewritten), &probe) != nil {
		return raw
	}
	return rewritten
}

// ---- 工具函数 ----

func shallowCopy(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

func shallowCopyWithout(source map[string]any, drop string) map[string]any {
	out := make(map[string]any, len(source))
	for k, v := range source {
		if k != drop {
			out[k] = v
		}
	}
	return out
}
