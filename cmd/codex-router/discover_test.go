package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// discover 的核心契约：以上游 /v1/models 为准拉全量列表，
// 坏凭据给明确错误而不是空列表。
func TestDiscoverFetchModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"glm-5.4"},{"id":"GLM-5.3"},{"id":"glm-5.3"}]}`))
	}))
	defer server.Close()

	models, err := discoverFetchModels(context.Background(), server.Client(), server.URL, "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("dedup failed: %v", models)
	}

	if _, err := discoverFetchModels(context.Background(), server.Client(), server.URL, "wrong"); err == nil {
		t.Fatal("rejected credential must be an error, not an empty list")
	}
}

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"GLM-5.3":      "glm-5.3",
		"glm-5.3:free": "glm-5.3",
		"deepseek-v4":  "deepseek-v4",
	}
	for in, want := range cases {
		if got := normalizeModelID(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
