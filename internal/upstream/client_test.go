package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abowloflrf/apid/internal/types"
)

// TestForwardHeaderPassthrough asserts business headers reach the upstream while
// auth, transport, and CDN/tracing headers are stripped, and that the configured
// apiKey overrides any client Authorization.
func TestForwardHeaderPassthrough(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/chat/completions", "secret-key")

	in := http.Header{}
	in.Set("X-Title", "my-app")                  // business header: forwarded
	in.Set("User-Agent", "client/1.0")           // forwarded
	in.Set("Authorization", "Bearer client-tok") // overridden by apiKey
	in.Set("X-Api-Key", "leak")                  // auth: stripped
	in.Set("Content-Length", "999")              // transport: stripped/reset
	in.Set("Connection", "keep-alive")           // hop-by-hop: stripped
	in.Set("Accept-Encoding", "gzip")            // stripped: Go transport manages it
	in.Set("X-Forwarded-For", "1.2.3.4")         // CDN prefix: stripped
	in.Set("CF-Connecting-IP", "1.2.3.4")        // CDN prefix: stripped

	resp, err := c.Forward(context.Background(), []byte(`{"model":"x"}`), in)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	resp.Body.Close()

	if v := got.Get("X-Title"); v != "my-app" {
		t.Errorf("X-Title = %q, want my-app", v)
	}
	if v := got.Get("User-Agent"); v != "client/1.0" {
		t.Errorf("User-Agent = %q, want client/1.0", v)
	}
	if v := got.Get("Authorization"); v != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want apiKey override", v)
	}
	for _, h := range []string{"X-Api-Key", "Connection", "X-Forwarded-For", "CF-Connecting-IP"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s = %q, want stripped", h, v)
		}
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
}

// TestForwardAuthFallback: with no configured apiKey, the client's Authorization
// is forwarded as-is.
func TestForwardAuthFallback(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/chat/completions", "")
	in := http.Header{}
	in.Set("Authorization", "Bearer client-tok")

	resp, err := c.Forward(context.Background(), []byte(`{}`), in)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	resp.Body.Close()

	if got != "Bearer client-tok" {
		t.Errorf("Authorization = %q, want client token passthrough", got)
	}
}

// TestChatCompletions asserts the thin wrapper marshals the request and POSTs it
// to Endpoint() with the configured auth, and surfaces the upstream response.
func TestChatCompletions(t *testing.T) {
	var gotBody []byte
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/chat/completions", "secret-key")
	req := &types.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []types.ChatMessage{{Role: "user", Content: "hi"}},
	}

	resp, err := c.ChatCompletions(context.Background(), req, http.Header{})
	if err != nil {
		t.Fatalf("ChatCompletions: %v", err)
	}
	resp.Body.Close()

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want apiKey", gotAuth)
	}
	var sent types.ChatRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream got non-JSON body %q: %v", gotBody, err)
	}
	if sent.Model != "deepseek-chat" || len(sent.Messages) != 1 {
		t.Errorf("upstream body = %+v, want model=deepseek-chat with 1 message", sent)
	}
}
