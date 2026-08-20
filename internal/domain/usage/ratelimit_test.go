package usage

import (
	"net/http"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// reset 三种表述归一：Go duration、裸秒、epoch。
func TestResetAtForms(t *testing.T) {
	base := testNow.UnixMilli()
	cases := []struct {
		in   string
		want int64
	}{
		{"2m59.56s", base + 2*60_000 + 59_560},
		{"7.66s", base + 7_660},
		{"100ms", base + 100},
		{"1h", base + 3_600_000},
		{"1h2m3s", base + 3_600_000 + 120_000 + 3_000},
		{"60", base + 60_000},             // 裸秒
		{"1786732800", 1786732800 * 1000}, // epoch 秒
		{"1786732800000", 1786732800000},  // epoch 毫秒
		{"garbage", 0},                    // 不可解析
		{"", 0},
	}
	for _, tc := range cases {
		if got := ResetAt(tc.in, testNow); got != tc.want {
			t.Errorf("ResetAt(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// 完整头解析：requests/tokens/retry-after 全维度。
func TestParseRateLimitHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Ratelimit-Limit-Requests", "50")
	headers.Set("X-Ratelimit-Remaining-Requests", "3")
	headers.Set("X-Ratelimit-Reset-Requests", "2m59.56s")
	headers.Set("X-Ratelimit-Limit-Tokens", "1000000")
	headers.Set("X-Ratelimit-Remaining-Tokens", "500000")
	headers.Set("X-Ratelimit-Reset-Tokens", "60")
	headers.Set("Retry-After", "30")

	snapshot := ParseRateLimitHeaders(headers, testNow)
	if snapshot == nil {
		t.Fatal("snapshot expected")
	}
	if snapshot.Requests == nil || *snapshot.Requests.Limit != 50 || *snapshot.Requests.Remaining != 3 {
		t.Errorf("requests window wrong: %+v", snapshot.Requests)
	}
	if snapshot.Tokens == nil || *snapshot.Tokens.Remaining != 500000 {
		t.Errorf("tokens window wrong: %+v", snapshot.Tokens)
	}
	if snapshot.RetryAt == "" {
		t.Error("retry-after must be captured")
	}
	// cooldown：remaining!=0 的窗口不触发；retry-after 30s 是唯一候选。
	wantRetry := testNow.UnixMilli() + 30_000
	if got := snapshot.CooldownUntil(); parseISO(time.UnixMilli(got).UTC().Format(time.RFC3339Nano)) != wantRetry {
		t.Errorf("cooldown = %d, want %d", got, wantRetry)
	}
}

// remaining==0 触发窗口 reset 作为 cooldown。
func TestCooldownFromExhaustedWindow(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Ratelimit-Remaining-Requests", "0")
	headers.Set("X-Ratelimit-Reset-Requests", "120")
	snapshot := ParseRateLimitHeaders(headers, testNow)
	want := testNow.UnixMilli() + 120_000
	if got := snapshot.CooldownUntil(); got != want {
		t.Errorf("cooldown = %d, want %d", got, want)
	}
}

// 无相关头 → nil。
func TestParseRateLimitHeadersNone(t *testing.T) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if snapshot := ParseRateLimitHeaders(headers, testNow); snapshot != nil {
		t.Errorf("no rate-limit headers must yield nil, got %+v", snapshot)
	}
}

// 持久化往返：last-write-wins，0600。
func TestRateLimitStoreRoundTrip(t *testing.T) {
	store := NewRateLimitStore(t.TempDir())
	headers := http.Header{}
	headers.Set("X-Ratelimit-Remaining-Requests", "42")
	snapshot := ParseRateLimitHeaders(headers, testNow)
	store.Record("zai-coding", snapshot, testNow)

	read := store.SnapshotFor("zai-coding")
	if read == nil || read.Requests == nil || *read.Requests.Remaining != 42 {
		t.Fatalf("round trip failed: %+v", read)
	}
	// 覆盖写。
	headers.Set("X-Ratelimit-Remaining-Requests", "7")
	store.Record("zai-coding", ParseRateLimitHeaders(headers, testNow), testNow)
	read = store.SnapshotFor("zai-coding")
	if read.Requests == nil || *read.Requests.Remaining != 7 {
		t.Errorf("last write must win: %+v", read)
	}
	if store.SnapshotFor("unknown") != nil {
		t.Error("unknown provider must read nil")
	}
}
