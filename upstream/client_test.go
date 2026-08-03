package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abowloflrf/apid/protocol"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

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

func TestForwardAnthropicXAPIKeyAuth(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/messages", "secret-key", WithXAPIKeyAuth())
	in := http.Header{}
	in.Set("Authorization", "Bearer client-tok")
	in.Set("X-Api-Key", "client-key")
	in.Set("Anthropic-Version", "2023-06-01")

	resp, err := c.Forward(context.Background(), []byte(`{"model":"claude"}`), in)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	resp.Body.Close()

	if v := got.Get("X-Api-Key"); v != "secret-key" {
		t.Errorf("X-Api-Key = %q, want configured key", v)
	}
	if v := got.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want stripped for Anthropic configured key", v)
	}
	if v := got.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want forwarded", v)
	}
}

func TestForwardAnthropicXAPIKeyFallback(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/messages", "", WithXAPIKeyAuth())
	in := http.Header{}
	in.Set("X-Api-Key", "client-key")

	resp, err := c.Forward(context.Background(), []byte(`{}`), in)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	resp.Body.Close()

	if got != "client-key" {
		t.Errorf("X-Api-Key = %q, want client key passthrough", got)
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
	req := &protocol.ChatRequest{
		Model:    "deepseek-chat",
		Messages: []protocol.ChatMessage{{Role: "user", Content: "hi"}},
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
	var sent protocol.ChatRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream got non-JSON body %q: %v", gotBody, err)
	}
	if sent.Model != "deepseek-chat" || len(sent.Messages) != 1 {
		t.Errorf("upstream body = %+v, want model=deepseek-chat with 1 message", sent)
	}
}

// TestForwardResponsesEndpoint: WithResponsesPath 给同一客户端加第二个 Responses
// 端点，ForwardResponsesWithQuery 打到该端点且鉴权语义与主端点一致。
func TestForwardResponsesEndpoint(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "/v1/chat/completions", "secret-key", WithResponsesPath("/v1/responses"))
	if !c.SupportsResponses() {
		t.Fatal("SupportsResponses() = false, want true")
	}
	if got := c.ResponsesEndpoint(); got != srv.URL+"/v1/responses" {
		t.Errorf("ResponsesEndpoint() = %q", got)
	}

	in := http.Header{}
	in.Set("Authorization", "Bearer client-tok")
	resp, err := c.ForwardResponsesWithQuery(context.Background(), []byte(`{"model":"gpt-x"}`), in, "beta=true")
	if err != nil {
		t.Fatalf("ForwardResponsesWithQuery: %v", err)
	}
	resp.Body.Close()

	if gotPath != "/v1/responses" {
		t.Errorf("path = %q, want /v1/responses", gotPath)
	}
	if gotQuery != "beta=true" {
		t.Errorf("query = %q, want beta=true", gotQuery)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want configured apiKey", gotAuth)
	}
	if string(gotBody) != `{"model":"gpt-x"}` {
		t.Errorf("body = %q, want verbatim", gotBody)
	}

	// 未配置 Responses 端点时调用应报错，而不是打到主端点。
	plain := New(srv.URL, "/v1/chat/completions", "")
	if _, err := plain.ForwardResponsesWithQuery(context.Background(), []byte(`{}`), http.Header{}, ""); err == nil {
		t.Fatal("ForwardResponsesWithQuery on a client without responses endpoint should error")
	}
}

func TestForwardCodexSubscriptionFixedTargetAndHeaders(t *testing.T) {
	body := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x01, 0x02, 0x03}
	var gotURL string
	var gotHeader http.Header
	var gotBody []byte
	calls := 0
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		gotURL = r.URL.String()
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		if r.GetBody != nil {
			t.Error("GetBody is set, want non-replayable POST")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
		}, nil
	})
	c := NewCodexSubscription(WithCodexSubscriptionRoundTripper(rt))

	h := http.Header{}
	h.Set("Authorization", "Bearer chatgpt-token")
	h.Set("X-Apid-Key", "local-secret")
	h.Set("X-Api-Key", "other-secret")
	h.Set("Cookie", "session=local")
	h.Set("X-Forwarded-For", "192.0.2.1")
	h.Set("ChatGPT-Account-Id", "account-id")
	h.Set("OpenAI-Beta", "responses=experimental")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Encoding", "zstd")

	resp, err := c.ForwardCodexSubscription(context.Background(), body, h,
		`access_token=do-not-log&next=https%3A%2F%2Fevil.example`, true)
	if err != nil {
		t.Fatalf("ForwardCodexSubscription: %v", err)
	}
	resp.Body.Close()

	wantURL := "https://chatgpt.com/backend-api/codex/responses/compact?access_token=do-not-log&next=https%3A%2F%2Fevil.example"
	if gotURL != wantURL {
		t.Errorf("URL = %q, want %q", gotURL, wantURL)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body changed: got %x want %x", gotBody, body)
	}
	for k, want := range map[string]string{
		"Authorization":      "Bearer chatgpt-token",
		"ChatGPT-Account-Id": "account-id",
		"OpenAI-Beta":        "responses=experimental",
		"Content-Type":       "application/octet-stream",
		"Content-Encoding":   "zstd",
	} {
		if got := gotHeader.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"X-Apid-Key", "X-Api-Key", "Cookie", "X-Forwarded-For"} {
		if got := gotHeader.Get(k); got != "" {
			t.Errorf("%s leaked: %q", k, got)
		}
	}
}

func TestForwardCodexSubscriptionBearerValidation(t *testing.T) {
	called := false
	c := NewCodexSubscription(WithCodexSubscriptionRoundTripper(roundTripperFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected call")
	})))

	cases := map[string]http.Header{
		"missing":        {},
		"empty":          {"Authorization": []string{"Bearer "}},
		"wrong scheme":   {"Authorization": []string{"Basic token"}},
		"duplicate":      {"Authorization": []string{"Bearer one", "Bearer two"}},
		"comma combined": {"Authorization": []string{"Bearer one, Bearer two"}},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			called = false
			if _, err := c.ForwardCodexSubscription(context.Background(), []byte(`{}`), h, "", false); err == nil {
				t.Fatal("expected Bearer validation error")
			}
			if called {
				t.Fatal("transport called after invalid Authorization")
			}
		})
	}
}

func TestCodexSubscriptionTransportSafety(t *testing.T) {
	c := NewCodexSubscription()
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", c.http.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("subscription transport uses an environment proxy")
	}
	if c.http.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	if err := c.http.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect error = %v", err)
	}
	if c.Endpoint() != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("Endpoint = %q", c.Endpoint())
	}
	if c.CompactEndpoint() != "https://chatgpt.com/backend-api/codex/responses/compact" {
		t.Fatalf("CompactEndpoint = %q", c.CompactEndpoint())
	}
}
