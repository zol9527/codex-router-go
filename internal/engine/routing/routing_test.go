package routing

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loyd/codex-router/internal/domain/registry"
	"github.com/loyd/codex-router/internal/domain/wire"
	"github.com/loyd/codex-router/internal/domain/wire/chatcompletion"
)

func TestRunAttemptTranslatesChatStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	model := &registry.Model{Slug: "test/model", Provider: "test"}
	runner := &Runner{Client: upstream.Client()}
	result, err := runner.RunAttempt(
		t.Context(), upstream.URL, nil, []byte(`{}`), model,
		chatcompletion.Protocol{}, wire.StreamOptions{SessionModel: model.Slug}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Translator == nil || !result.Translator.HasContent() {
		t.Fatal("translated stream should report content")
	}
	if got := string(result.Events.Bytes()); !strings.Contains(got, "response.output_text.delta") {
		t.Fatalf("translated events missing output delta: %s", got)
	}
}

func TestRunAttemptExposesUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"busy"}`)
	}))
	defer upstream.Close()

	model := &registry.Model{Slug: "test/model", Provider: "test"}
	_, err := (&Runner{Client: upstream.Client()}).RunAttempt(
		t.Context(), upstream.URL, nil, []byte(`{}`), model,
		chatcompletion.Protocol{}, wire.StreamOptions{}, nil,
	)
	var failure *UpstreamFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want UpstreamFailure", err)
	}
	if failure.Status != http.StatusTooManyRequests || failure.RetryAfter != 7 || failure.BodyText != `{"error":"busy"}` {
		t.Fatalf("unexpected failure: %+v", failure)
	}
}
