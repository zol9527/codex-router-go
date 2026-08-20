package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 错误翻译完整版（移植 error-translation.mjs）：把上游错误体重写成
// 命名 provider 的错误，识别配额耗尽 / 套餐不含 API 两类需要不同
// 处置指引的形态。

const detailLimit = 300

var routingNoise = []*regexp.Regexp{
	regexp.MustCompile(`(?s)\.?\s*Received Model Group=.*$`),
	regexp.MustCompile(`(?s)\s*Available Model Group Fallbacks=.*$`),
}

var wrapperPrefixes = []*regexp.Regexp{
	regexp.MustCompile(`^litellm\.[A-Za-z]+:\s*`),
	regexp.MustCompile(`^[A-Za-z]+Error:\s*`),
	regexp.MustCompile(`^[A-Za-z]+Exception\s*-\s*`),
}

var quotaPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)insufficient[_\s]quota`),
	regexp.MustCompile(`(?i)exceeded your current quota`),
	regexp.MustCompile(`(?i)insufficient (?:balance|credits?)`),
	regexp.MustCompile(`(?i)credit balance is too low`),
	regexp.MustCompile(`(?i)(?:no|any|out of) credits`),
	regexp.MustCompile(`(?i)usage limits? (?:reached|exceeded|hit)`),
	regexp.MustCompile(`(?i)reached your (?:usage|monthly|daily) limit`),
	regexp.MustCompile(`(?i)(?:monthly|daily|plan) usage limit`),
	regexp.MustCompile(`(?i)purchase extra usage`),
	regexp.MustCompile(`(?i)upgrade your plan`),
	regexp.MustCompile(`(?i)quota\b[^.]*\bexhausted`),
	regexp.MustCompile(`(?i)quota (?:exceeded|exhausted|will be refreshed)`),
	regexp.MustCompile(`(?i)arrears`),
	regexp.MustCompile(`(?i)balance (?:is )?(?:too low|not enough|insufficient)`),
	regexp.MustCompile(`余额不足`),
	regexp.MustCompile(`欠费`),
	regexp.MustCompile(`额度(?:不足|已用完)`),
}

var entitlementPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)plan does(?:n't| not) include`),
	regexp.MustCompile(`(?i)not included (?:in|with) your .{0,40}plan`),
	regexp.MustCompile(`(?i)upgrade to [\w\s]{1,30}(?:or higher|plan)`),
	regexp.MustCompile(`(?i)requires? (?:the |a |an )?[\w\s]{1,30}plan`),
	regexp.MustCompile(`(?i)plan does(?:n't| not) support`),
	regexp.MustCompile(`(?i)no api access`),
}

// contextOverflowPatterns 识别上游"输入超窗"错误的各家措辞。
// 思想来源：opencode v2 packages/llm/src/provider-error.ts 的
// isContextOverflow 模式库（28 条正则 + 限流排除项，防把限流文案
// 误判为超窗）——正则集合按本 router 实际触达的供应商措辞整理：
// OpenAI "context_length_exceeded"、Anthropic "prompt is too long"、
// Google/DeepSeek "maximum context length"、MiniMax/Z.ai 中英混排。
// 命中后错误规范化为 OpenAI 官方 code（context_length_exceeded），
// Codex 客户端对该 code 有本地化处置（提示压缩会话）。
var contextOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)context[_\s-]?length[_\s-]?(?:exceeded|is too long)`),
	regexp.MustCompile(`(?i)context[_\s-]?window.{0,40}(?:exceed|too (?:long|large)|surpass)`),
	regexp.MustCompile(`(?i)maximum context (?:length|size|window)`),
	regexp.MustCompile(`(?i)prompt is too long`),
	regexp.MustCompile(`(?i)(?:input|prompt|request).{0,30}(?:exceeds?|larger than).{0,30}(?:maximum|limit|context|token|window|allowed)`),
	regexp.MustCompile(`(?i)too many (?:input |total |prompt )?tokens`),
	regexp.MustCompile(`(?i)(?:input|prompt)[_\s]?tokens?.{0,25}(?:limit|exceed|max)`),
	regexp.MustCompile(`(?i)exceeds? the (?:maximum|model's|context|token|input)`),
	regexp.MustCompile(`(?i)reduce (?:the )?(?:prompt|input|length|context|message)`),
	regexp.MustCompile(`(?i)上下文.{0,12}(?:过长|超出|超限|超过)|超过.{0,12}(?:上下文|长度限制)|(?:输入|提示词).{0,10}过长`),
	regexp.MustCompile(`(?i)最长.{0,8}输入|token.{0,6}上限`),
}

// overflowExclusions 是超窗判定的排除项：这些文案命中时是限流而非
// 超窗（部分厂商的限流错误也带 "tokens" 字样，如 RPM/TPM 限制）。
var overflowExclusions = []*regexp.Regexp{
	regexp.MustCompile(`(?i)rate.?limit`),
	regexp.MustCompile(`(?i)too many requests`),
	regexp.MustCompile(`(?i)(?:requests per minute|rpm|tokens per minute|tpm)`),
}

// classifyContextOverflow 判定错误文本是否为输入超窗（先排除限流）。
func classifyContextOverflow(detail string) bool {
	for _, pattern := range overflowExclusions {
		if pattern.MatchString(detail) {
			return false
		}
	}
	for _, pattern := range contextOverflowPatterns {
		if pattern.MatchString(detail) {
			return true
		}
	}
	return false
}

// parseUpstreamError 从各种 provider 错误形状里挖出人类可读消息。
func parseUpstreamError(bodyText string) (message string, errType string) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(bodyText), &parsed); err != nil {
		return bodyText, ""
	}
	errorField, _ := parsed["error"].(map[string]any)
	if s, ok := errorField["message"].(string); ok {
		message = s
	}
	if message == "" {
		if s, ok := errorField["type"].(string); ok {
			errType = s
		}
	}
	if baseResp, ok := parsed["base_resp"].(map[string]any); ok && message == "" {
		if s, ok := baseResp["status_msg"].(string); ok {
			message = s
		}
	}
	if message == "" {
		if s, ok := parsed["message"].(string); ok {
			message = s
		}
	}
	if message == "" {
		if s, ok := parsed["detail"].(string); ok {
			message = s
		}
	}
	if message == "" {
		message = bodyText
	}
	if errType == "" {
		if s, ok := errorField["type"].(string); ok {
			errType = s
		} else if s, ok := errorField["status"].(string); ok {
			errType = s
		}
	}
	return message, errType
}

// extractUpstreamDetail 剥掉路由噪音与包装前缀，限长。
func extractUpstreamDetail(bodyText string) string {
	message, _ := parseUpstreamError(bodyText)
	for _, pattern := range routingNoise {
		message = pattern.ReplaceAllString(message, "")
	}
	for changed := true; changed; {
		changed = false
		for _, pattern := range wrapperPrefixes {
			next := pattern.ReplaceAllString(message, "")
			if next != message {
				message = next
				changed = true
			}
		}
	}
	message = strings.TrimSpace(message)
	if utf8.RuneCountInString(message) > detailLimit {
		runes := []rune(message)
		message = string(runes[:detailLimit])
	}
	return message
}

// classifyQuota 按消息文本分类配额形态。
func classifyQuota(detail string) string {
	for _, pattern := range quotaPatterns {
		if pattern.MatchString(detail) {
			return "provider_quota_exhausted"
		}
	}
	for _, pattern := range entitlementPatterns {
		if pattern.MatchString(detail) {
			return "plan_entitlement_required"
		}
	}
	return ""
}

// translateProviderError 产出最终错误体。
func translateProviderError(status int, bodyText, modelName, providerName, providerID string, retryAfter int) map[string]any {
	detail := extractUpstreamDetail(bodyText)
	code := classifyQuota(detail)
	if code == "" && classifyContextOverflow(detail) {
		// 超窗规范化为 OpenAI 官方 code：Codex 对 context_length_
		// exceeded 有本地化处置（提示压缩会话），泛化的
		// provider_api_error_400 会被当作普通上游错误展示。
		code = "context_length_exceeded"
	}
	if code == "" {
		code = fmt.Sprintf("provider_api_error_%d", status)
	}
	guidance := ""
	switch code {
	case "provider_quota_exhausted":
		guidance = " The provider's usage limit is exhausted; top up or wait for the window to reset."
	case "plan_entitlement_required":
		guidance = " The current plan does not include API access; no key or setup change can fix this."
	case "context_length_exceeded":
		guidance = " The conversation exceeds the model's context window; compact the session (/compact) or trim history before retrying."
	case "provider_api_error_401", "provider_api_error_403":
		guidance = " Check the provider credential (codex-router control credential " + providerID + ")."
	}
	if retryAfter > 0 {
		guidance += fmt.Sprintf(" Retry-After: %ds.", retryAfter)
	}
	message := fmt.Sprintf("%s (via %s) failed: %s.%s", modelName, providerName, detail, guidance)
	return map[string]any{
		"error": map[string]any{
			"type":     code,
			"code":     code,
			"provider": providerName,
			"message":  message,
		},
	}
}
