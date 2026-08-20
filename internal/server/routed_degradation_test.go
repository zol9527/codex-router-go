package server

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/loyd/codex-router/internal/registry"
	"github.com/loyd/codex-router/internal/wire"
)

// 降级日志限流：同一签名窗口内只记一次，被抑制的次数累计进下一条
// （suppressed=N）；不同签名互不影响。日志是排障线索，只降噪不丢信息。
func TestDegradationLogRateLimited(t *testing.T) {
	prev := degradationLogInterval
	degradationLogInterval = 5 * time.Millisecond
	defer func() {
		degradationLogInterval = prev
		degradationLogMu.Lock()
		degradationLogLast = map[string]time.Time{}
		degradationLogSuppressed = map[string]int{}
		degradationLogMu.Unlock()
	}()

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	mystery := &wire.Request{OmittedItemTypes: []string{"mystery_item"}}
	other := &wire.Request{OmittedPartTypes: []string{"mystery_part"}}
	model := &registry.Model{Slug: "test/model"}

	logTranslationDegradation(mystery, model)
	logTranslationDegradation(mystery, model)
	logTranslationDegradation(mystery, model)
	// 不同签名不受同一窗口抑制。
	logTranslationDegradation(other, model)
	if n := strings.Count(buf.String(), "chat translation degraded"); n != 2 {
		t.Fatalf("expected 1 line per signature within window, got %d: %s", n, buf.String())
	}

	time.Sleep(6 * time.Millisecond)
	logTranslationDegradation(mystery, model)
	out := buf.String()
	if !strings.Contains(out, "suppressed=2/2s") {
		t.Fatalf("suppressed count missing after window: %s", out)
	}
}

// 无降级形状时零输出、零记账。
func TestDegradationLogSilentWhenClean(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	logTranslationDegradation(&wire.Request{}, &registry.Model{Slug: "test/model"})
	logTranslationDegradation(&wire.Request{}, nil)
	if buf.Len() != 0 {
		t.Fatalf("clean request must not log: %s", buf.String())
	}
}
