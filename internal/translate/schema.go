package translate

import (
	"encoding/json"
	"reflect"
)

// providerToolSchema 归一，移植自 tool-schema-root.mjs。
//
// 两类严格上游拒绝点：
//  1. Moonshot/Kimi 类：enum/const literal 与自己节点声明的 type 矛盾
//     （如 [string] 里出现 true），且拒绝整个请求而非单个工具；
//  2. xAI 类：union 根（anyOf/oneOf/allOf）无论声明什么 type 都拒绝。
//
// 归一只作用于发给 provider 的副本；客户端的 inputSchema 原样保留。

const maxLiteralDepth = 32

var schemaMapKeywords = []string{"properties", "patternProperties", "$defs", "definitions"}
var schemaListKeywords = []string{"anyOf", "oneOf", "allOf", "prefixItems"}
var schemaChildKeywords = []string{"items", "additionalProperties", "propertyNames", "contains", "not", "if", "then", "else"}
var unionKeywords = []string{"anyOf", "oneOf", "allOf"}

// ProviderToolSchema：literal 对齐自身类型 + union 根合并为对象根。
// 无需改动时返回原引用。
func ProviderToolSchema(schema any) any {
	normalized, _ := normalizeSchemaLiterals(schema, 0)
	if m, ok := normalized.(map[string]any); ok && hasRootUnion(m) {
		return objectRootToolSchema(m)
	}
	return normalized
}

func hasRootUnion(schema map[string]any) bool {
	for _, keyword := range unionKeywords {
		if _, ok := schema[keyword].([]any); ok {
			return true
		}
	}
	return false
}

func hasObjectRoot(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	if hasRootUnion(schema) {
		return false
	}
	if schema["type"] == "object" {
		return true
	}
	_, hasProperties := schema["properties"].(map[string]any)
	return hasProperties
}

// jsonTypeOf 返回 JSON 值的 schema 类型名。
func jsonTypeOf(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		return "number"
	case float64, int, int64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return ""
	}
}

func declaredTypes(schema map[string]any) []string {
	if t, ok := schema["type"].(string); ok {
		return []string{t}
	}
	var out []string
	if list, ok := schema["type"].([]any); ok {
		for _, entry := range list {
			if s, ok := entry.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func matchesDeclaredType(value any, types []string) bool {
	actual := jsonTypeOf(value)
	if actual == "" {
		return false
	}
	for _, t := range types {
		if t == actual {
			return true
		}
	}
	return false
}

// normalizeSchemaLiterals 递归丢弃与自身节点声明类型矛盾的 literal。
// 第二返回值说明是否产生过改写；未改写时第一返回值即原引用
// （零拷贝，客户端对象绝不原地变动）。
func normalizeSchemaLiterals(schema any, depth int) (any, bool) {
	m, ok := schema.(map[string]any)
	if !ok || depth > maxLiteralDepth {
		return schema, false
	}
	next := m
	copied := false
	touch := func() {
		if !copied {
			next = shallowCopy(m)
			copied = true
		}
	}
	changedAny := false

	types := declaredTypes(m)
	if len(types) > 0 {
		if enum, ok := m["enum"].([]any); ok {
			var kept []any
			for _, value := range enum {
				if matchesDeclaredType(value, types) {
					kept = append(kept, value)
				}
			}
			if len(kept) != len(enum) {
				touch()
				if len(kept) > 0 {
					next["enum"] = kept
				} else {
					delete(next, "enum")
				}
				changedAny = true
			}
		}
		if constValue, has := m["const"]; has {
			if !matchesDeclaredType(constValue, types) {
				touch()
				delete(next, "const")
				changedAny = true
			}
		}
	}
	for _, keyword := range schemaMapKeywords {
		node, ok := m[keyword].(map[string]any)
		if !ok {
			continue
		}
		rewritten := map[string]any{}
		changed := false
		for name, child := range node {
			sanitized, childChanged := normalizeSchemaLiterals(child, depth+1)
			if childChanged {
				changed = true
			}
			rewritten[name] = sanitized
		}
		if changed {
			touch()
			next[keyword] = rewritten
			changedAny = true
		}
	}
	for _, group := range [][]string{schemaListKeywords, schemaChildKeywords} {
		for _, keyword := range group {
			switch node := m[keyword].(type) {
			case []any:
				rewritten := make([]any, len(node))
				changed := false
				for i, child := range node {
					sanitized, childChanged := normalizeSchemaLiterals(child, depth+1)
					if childChanged {
						changed = true
					}
					rewritten[i] = sanitized
				}
				if changed {
					touch()
					next[keyword] = rewritten
					changedAny = true
				}
			case map[string]any:
				if sanitized, childChanged := normalizeSchemaLiterals(node, depth+1); childChanged {
					touch()
					next[keyword] = sanitized
					changedAny = true
				}
			}
		}
	}
	if !changedAny {
		return schema, false
	}
	return next, true
}

// objectBranches 递归展开 union，收集对象根分支。
// 循环引用防护用 reflect 指针身份对比（Go 的 map 不能 ==）。
func objectBranches(schema map[string]any, seen []uintptr) []map[string]any {
	self := reflect.ValueOf(schema).Pointer()
	for _, visited := range seen {
		if visited == self {
			return nil
		}
	}
	seen = append(seen, self)
	var branches []map[string]any
	for _, keyword := range unionKeywords {
		list, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for _, raw := range list {
			branch, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if hasObjectRoot(branch) {
				branches = append(branches, branch)
			} else {
				branches = append(branches, objectBranches(branch, seen)...)
			}
		}
	}
	return branches
}

// objectRootToolSchema 把 union 根合并成普通对象 schema：
//   - 根级 properties 对每个分支生效，优先于分支内同名定义；
//   - required 只保留"根级要求 ∪ 全部分支共同要求"—— 视图分支要求的
//     字段对删除分支是可选的，标成必需会拒绝 app 本可接受的调用；
//   - additionalProperties: true —— 合并后的对象无法描述分支判别字段。
func objectRootToolSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if hasObjectRoot(schema) {
		return schema
	}
	branches := objectBranches(schema, nil)
	properties := map[string]any{}
	if rootProps, ok := schema["properties"].(map[string]any); ok {
		for name, property := range rootProps {
			properties[name] = property
		}
	}
	for _, branch := range branches {
		if branchProps, ok := branch["properties"].(map[string]any); ok {
			for name, property := range branchProps {
				if _, exists := properties[name]; !exists {
					properties[name] = property
				}
			}
		}
	}
	var required []string
	seenRequired := map[string]bool{}
	appendRequired := func(values []any) {
		for _, raw := range values {
			if name, ok := raw.(string); ok && !seenRequired[name] {
				seenRequired[name] = true
				required = append(required, name)
			}
		}
	}
	if rootRequired, ok := schema["required"].([]any); ok {
		appendRequired(rootRequired)
	}
	if len(branches) > 0 {
		shared := map[string]bool{}
		if first, ok := branches[0]["required"].([]any); ok {
			for _, raw := range first {
				if name, ok := raw.(string); ok {
					shared[name] = true
				}
			}
		}
		for _, branch := range branches[1:] {
			next := map[string]bool{}
			if list, ok := branch["required"].([]any); ok {
				for _, raw := range list {
					name, isString := raw.(string)
					if isString && shared[name] {
						next[name] = true
					}
				}
			}
			shared = next
		}
		for name := range shared {
			if !seenRequired[name] {
				seenRequired[name] = true
				required = append(required, name)
			}
		}
	}
	out := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": true,
	}
	if v, ok := schema["$schema"]; ok {
		out["$schema"] = v
	}
	if v, ok := schema["$defs"]; ok {
		out["$defs"] = v
	}
	if v, ok := schema["definitions"]; ok {
		out["definitions"] = v
	}
	if v, ok := schema["description"].(string); ok && v != "" {
		out["description"] = v
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
