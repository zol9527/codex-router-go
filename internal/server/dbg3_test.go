package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebugSilentRetryReal(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Recovered.\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	srv, ts := newTestServer(t)
	srv.opt.Registry.Providers["zai-coding"].BaseURL = upstream.URL
	srv.opt.Registry.Providers["zai-coding"].BaseURLEnv = ""
	t.Setenv("ZAI_API_KEY", "")
	callerKey, _ := srv.opt.State.CallerKey()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+CallerPathPrefix+"/"+callerKey+"/v1/responses",
		strings.NewReader(`{"model":"zai-coding/glm-5.3","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	fmt.Printf("STATUS=%d CALLS=%d\n", resp.StatusCode, calls)
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	fmt.Printf("BODY[0:%d]=%.300s\n", n, buf[:n])
}
