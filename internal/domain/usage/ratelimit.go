package usage

// rate-limit header 收割，移植自 rate-limit-headers.mjs 与
// rate-limit-state.mjs。被动发现：多数 OpenAI 兼容上游在响应头里带
// x-ratelimit-*（Anthropic 是 anthropic-ratelimit-*），读它们零额外
// 请求。持久化是 last-write-wins 的小文档（rate-limits.json），
// 因为只有当前窗口有意义。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateWindow 是一个维度的限额窗口（requests 或 tokens）。
type RateWindow struct {
	Limit     *int64 `json:"limit,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
	ResetAt   string `json:"resetAt,omitempty"`
}

// RateSnapshot 是一次响应携带的全部限额事实。
type RateSnapshot struct {
	Requests *RateWindow `json:"requests,omitempty"`
	Tokens   *RateWindow `json:"tokens,omitempty"`
	RetryAt  string      `json:"retryAt,omitempty"`
}

// parseGoDuration 手写扫描 Go 风格 duration 的子集
// （[Nh][Nm][Nms|Ns]，段序固定，小数允许）。Go 的 RE2 不支持
// 负向前瞻（原版用 m(?!s) 区分 m 与 ms），所以手动扫描。
func parseGoDuration(text string) (int64, bool) {
	total := int64(0)
	rest := text
	order := 0 // h=1 < m=2 < s/ms=3，保证段序
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, false
		}
		number, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, false
		}
		rest = rest[i:]
		unit := ""
		for _, candidate := range []string{"ms", "h", "m", "s"} {
			if strings.HasPrefix(rest, candidate) {
				unit = candidate
				rest = rest[len(candidate):]
				break
			}
		}
		if unit == "" {
			return 0, false
		}
		rank := map[string]int{"h": 1, "m": 2, "ms": 3, "s": 3}[unit]
		if rank <= order {
			return 0, false
		}
		order = rank
		switch unit {
		case "h":
			total += int64(number * 3_600_000)
		case "m":
			total += int64(number * 60_000)
		case "ms":
			total += int64(number)
		case "s":
			total += int64(number * 1000)
		}
	}
	return total, true
}

// ResetAt 把三种 reset 表述归一为 epoch 毫秒：Go 风格 duration
// （"2m59.56s"）、裸秒数（"60"，或足够大时是 epoch）、绝对时间戳。
// 无法解析返回 0。
func ResetAt(value string, now time.Time) int64 {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0
	}
	// 裸数字。
	if bare, err := strconv.ParseFloat(text, 64); err == nil {
		if bare >= 1_000_000_000 {
			if bare >= 1e12 {
				return int64(bare)
			}
			return int64(bare * 1000)
		}
		if bare >= 0 {
			return now.UnixMilli() + int64(bare*1000)
		}
		return 0
	}
	if ms, ok := parseGoDuration(strings.ToLower(text)); ok {
		return now.UnixMilli() + ms
	}
	if absolute, err := time.Parse(time.RFC1123, text); err == nil {
		return absolute.UnixMilli()
	}
	if absolute, err := time.Parse(time.RFC1123Z, text); err == nil {
		return absolute.UnixMilli()
	}
	return 0
}

func orZero(value string) string {
	if value == "" {
		return "0"
	}
	return value
}

func headerValue(headers map[string][]string, keys ...string) string {
	for _, key := range keys {
		if values, ok := headers[key]; ok && len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			return values[0]
		}
	}
	return ""
}

func countOf(value string) *int64 {
	if value == "" {
		return nil
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || number < 0 {
		return nil
	}
	rounded := int64(number)
	return &rounded
}

func isoMillis(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

func windowFromHeaders(headers map[string][]string, limitKeys, remainingKeys, resetKeys []string, now time.Time) *RateWindow {
	limit := countOf(headerValue(headers, limitKeys...))
	remaining := countOf(headerValue(headers, remainingKeys...))
	reset := isoMillis(ResetAt(headerValue(headers, resetKeys...), now))
	if limit == nil && remaining == nil && reset == "" {
		return nil
	}
	return &RateWindow{Limit: limit, Remaining: remaining, ResetAt: reset}
}

// ParseRateLimitHeaders 从响应头提取限额快照；无相关头返回 nil。
func ParseRateLimitHeaders(headers map[string][]string, now time.Time) *RateSnapshot {
	requests := windowFromHeaders(headers,
		[]string{"X-Ratelimit-Limit-Requests", "Anthropic-Ratelimit-Requests-Limit", "X-Ratelimit-Limit"},
		[]string{"X-Ratelimit-Remaining-Requests", "Anthropic-Ratelimit-Requests-Remaining", "X-Ratelimit-Remaining"},
		[]string{"X-Ratelimit-Reset-Requests", "Anthropic-Ratelimit-Requests-Reset", "X-Ratelimit-Reset"},
		now)
	tokens := windowFromHeaders(headers,
		[]string{"X-Ratelimit-Limit-Tokens", "Anthropic-Ratelimit-Tokens-Limit"},
		[]string{"X-Ratelimit-Remaining-Tokens", "Anthropic-Ratelimit-Tokens-Remaining"},
		[]string{"X-Ratelimit-Reset-Tokens", "Anthropic-Ratelimit-Tokens-Reset"},
		now)
	retryAt := isoMillis(ResetAt(headerValue(headers, "Retry-After"), now))
	if requests == nil && tokens == nil && retryAt == "" {
		return nil
	}
	return &RateSnapshot{Requests: requests, Tokens: tokens, RetryAt: retryAt}
}

// CooldownUntil 是该 provider 值得重试的最早时刻（remaining==0 的
// reset 或 Retry-After 的最晚者）；无任何限流信号返回 0。
func (s *RateSnapshot) CooldownUntil() int64 {
	if s == nil {
		return 0
	}
	var candidates []int64
	if s.RetryAt != "" {
		if ms := parseISO(s.RetryAt); ms > 0 {
			candidates = append(candidates, ms)
		}
	}
	if s.Requests != nil && s.Requests.Remaining != nil && *s.Requests.Remaining == 0 && s.Requests.ResetAt != "" {
		if ms := parseISO(s.Requests.ResetAt); ms > 0 {
			candidates = append(candidates, ms)
		}
	}
	if s.Tokens != nil && s.Tokens.Remaining != nil && *s.Tokens.Remaining == 0 && s.Tokens.ResetAt != "" {
		if ms := parseISO(s.Tokens.ResetAt); ms > 0 {
			candidates = append(candidates, ms)
		}
	}
	if len(candidates) == 0 {
		return 0
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] > candidates[j] })
	return candidates[0]
}

func parseISO(value string) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

// ---- 持久化（rate-limits.json，last-write-wins）----

const maxRateLimitProviders = 64

type rateLimitStore struct {
	mu   sync.Mutex
	path string
}

// RateLimitStore 管理限额快照文档。
type RateLimitStore = rateLimitStore

// NewRateLimitStore 创建 store（state 目录）。
func NewRateLimitStore(stateDir string) *RateLimitStore {
	return &rateLimitStore{path: filepath.Join(stateDir, "rate-limits.json")}
}

type observedSnapshot struct {
	RateSnapshot
	ObservedAt string `json:"observedAt"`
}

// Record 记录 provider 的最新快照。失败静默：限额遥测绝不打断请求。
func (s *RateLimitStore) Record(providerID string, snapshot *RateSnapshot, at time.Time) {
	if s == nil || snapshot == nil || strings.TrimSpace(providerID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	document := s.readLocked()
	document[strings.TrimSpace(providerID)] = observedSnapshot{
		RateSnapshot: *snapshot, ObservedAt: at.UTC().Format(time.RFC3339Nano),
	}
	// 按 observedAt 新旧排序裁剪到上限。
	type entry struct {
		id    string
		value observedSnapshot
	}
	entries := make([]entry, 0, len(document))
	for id, value := range document {
		entries = append(entries, entry{id, value})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.Compare(entries[i].value.ObservedAt, entries[j].value.ObservedAt) > 0
	})
	if len(entries) > maxRateLimitProviders {
		entries = entries[:maxRateLimitProviders]
	}
	trimmed := map[string]observedSnapshot{}
	for _, e := range entries {
		trimmed[e.id] = e.value
	}
	s.writeLocked(trimmed)
}

// SnapshotFor 读取 provider 的最新快照。
func (s *RateLimitStore) SnapshotFor(providerID string) *RateSnapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	document := s.readLocked()
	observed, ok := document[strings.TrimSpace(providerID)]
	if !ok {
		return nil
	}
	snapshot := observed.RateSnapshot
	return &snapshot
}

func (s *RateLimitStore) readLocked() map[string]observedSnapshot {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]observedSnapshot{}
	}
	var document map[string]observedSnapshot
	if err := json.Unmarshal(raw, &document); err != nil {
		// 损坏文件绝不拖垮路由；下一次带头的响应会重建它。
		return map[string]observedSnapshot{}
	}
	return document
}

func (s *RateLimitStore) writeLocked(document map[string]observedSnapshot) {
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, append(raw, '\n'), 0o600); err != nil {
		return
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return
	}
	os.Chmod(s.path, 0o600)
}
