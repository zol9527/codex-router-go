package nativebackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBackendRelayHidesEndpointAndFiltersHeaders(t *testing.T) {
	var seenPath string
	var seenAuth string
	var seenExtra string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenExtra = r.Header.Get("X-Router-Internal")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer upstream")
	header.Set("X-Router-Internal", "secret")
	backend := New(upstream.URL, upstream.Client(), 0, func(string) bool { return false })
	resp, err := backend.Relay(context.Background(), RelayRequest{
		Route: "/v1/responses", Body: []byte("{}"), Header: header, Compress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if seenPath != "/responses" {
		t.Fatalf("path = %q, want /responses", seenPath)
	}
	if seenAuth != "Bearer upstream" {
		t.Fatalf("Authorization was not forwarded: %q", seenAuth)
	}
	if seenExtra != "" {
		t.Fatalf("non-whitelisted header leaked: %q", seenExtra)
	}
	if resp.Status != http.StatusTeapot {
		t.Fatalf("status = %d", resp.Status)
	}
}

func TestPostResponsesRejectsOversizedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 16))
	}))
	defer upstream.Close()

	backend := New(upstream.URL, upstream.Client(), 0, nil)
	if _, err := backend.PostResponses(context.Background(), Request{Body: map[string]any{}, MaxBytes: 8}); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestBearerToken(t *testing.T) {
	if got := BearerToken("bearer abc"); got != "abc" {
		t.Fatalf("token = %q", got)
	}
	if got := BearerToken("basic abc"); got != "" {
		t.Fatalf("non-bearer token = %q", got)
	}
}
