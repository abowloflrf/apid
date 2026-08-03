package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/store"
	"github.com/abowloflrf/apid/upstream"
)

type subscriptionRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f subscriptionRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func subscriptionConfig(localKey, traceDir string) config.Config {
	passthrough := ""
	return config.Config{
		Listen:       "127.0.0.1:19092",
		TraceDir:     traceDir,
		ClientAPIKey: localKey,
		Upstreams: []config.Upstream{{
			Name:     "codex-subscription",
			Protocol: config.ProtoResponses,
			BaseURL:  config.CodexSubscriptionBaseURL,
			Path:     config.CodexSubscriptionPath,
			AuthMode: config.AuthModeCodexSubscription,
		}},
		Routes: []config.Route{
			{
				Path:          "/codex/v1/responses",
				InputProtocol: config.ProtoResponses,
				Models: []config.ModelRule{{
					Match: "*", Upstream: "codex-subscription", Model: &passthrough,
				}},
			},
			{
				Path:          "/codex/v1/responses/compact",
				InputProtocol: config.ProtoResponses,
				Operation:     config.RouteOperationResponsesCompact,
				Models: []config.ModelRule{{
					Match: "*", Upstream: "codex-subscription", Model: &passthrough,
				}},
			},
		},
	}
}

func newSubscriptionTestServer(t *testing.T, localKey, traceDir string, rt http.RoundTripper, log *slog.Logger) *Server {
	t.Helper()
	srv := New(subscriptionConfig(localKey, traceDir), nil, log)
	srv.upstreams["codex-subscription"].client = upstream.NewCodexSubscription(
		upstream.WithCodexSubscriptionRoundTripper(rt),
	)
	t.Cleanup(srv.Close)
	return srv
}

func TestSubscriptionRouteAuthFixedTargetAndRedaction(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	traceDir := t.TempDir()
	requestBody := []byte(`{"model":"gpt-5","input":"hello"}`)
	var gotURL string
	var gotHeader http.Header
	var gotBody []byte
	rt := subscriptionRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":      []string{"application/json"},
				"Openai-Request-Id": []string{"req_safe"},
				"X-Ratelimit-Limit": []string{"10"},
				"Set-Cookie":        []string{"upstream=secret"},
				"Content-Length":    []string{"999"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	})
	srv := newSubscriptionTestServer(t, "local-key", traceDir, rt, logger)

	req := httptest.NewRequest(http.MethodPost,
		"/codex/v1/responses?access_token=query-secret&next=https%3A%2F%2Fevil.example",
		bytes.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer openai-secret")
	req.Header.Set("X-Apid-Key", "local-key")
	req.Header.Set("Cookie", "local=cookie-secret")
	req.Header.Set("ChatGPT-Account-Id", "account-secret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantURL := "https://chatgpt.com/backend-api/codex/responses?access_token=query-secret&next=https%3A%2F%2Fevil.example"
	if gotURL != wantURL {
		t.Errorf("upstream URL = %q, want %q", gotURL, wantURL)
	}
	if !bytes.Equal(gotBody, requestBody) {
		t.Errorf("request body changed: got %q want %q", gotBody, requestBody)
	}
	if gotHeader.Get("Authorization") != "Bearer openai-secret" {
		t.Errorf("Authorization = %q", gotHeader.Get("Authorization"))
	}
	for _, key := range []string{"X-Apid-Key", "Cookie"} {
		if got := gotHeader.Get(key); got != "" {
			t.Errorf("%s leaked upstream: %q", key, got)
		}
	}
	if gotHeader.Get("ChatGPT-Account-Id") != "account-secret" {
		t.Error("ChatGPT-Account-Id was not forwarded")
	}
	if rec.Header().Get("Openai-Request-Id") != "req_safe" || rec.Header().Get("X-Ratelimit-Limit") != "10" {
		t.Errorf("safe diagnostic headers missing: %v", rec.Header())
	}
	for _, key := range []string{"Set-Cookie", "Content-Length"} {
		if got := rec.Header().Get(key); got != "" {
			t.Errorf("unsafe response header %s = %q", key, got)
		}
	}
	for _, secret := range []string{"query-secret", "openai-secret", "cookie-secret", "account-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("secret %q leaked to logs: %s", secret, logs.String())
		}
	}
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("subscription route wrote TRACE files: %v", entries)
	}
}

func TestSubscriptionRouteRequiresSeparateLocalAndOpenAIKeys(t *testing.T) {
	calls := 0
	rt := subscriptionRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	srv := newSubscriptionTestServer(t, "local-key", "", rt, nil)

	cases := []struct {
		name    string
		headers http.Header
	}{
		{"Authorization cannot authenticate apid", http.Header{"Authorization": []string{"Bearer local-key"}}},
		{"missing OpenAI bearer", http.Header{"X-Apid-Key": []string{"local-key"}}},
		{"duplicate bearer", http.Header{"X-Apid-Key": []string{"local-key"}, "Authorization": []string{"Bearer one", "Bearer two"}}},
		{"comma bearer", http.Header{"X-Apid-Key": []string{"local-key"}, "Authorization": []string{"Bearer one, Bearer two"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses", strings.NewReader(`{}`))
			req.Header = tc.headers
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestSubscriptionCompactAndUnknownStream(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	rt := subscriptionRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/backend-api/codex/responses/compact" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Content-Encoding") != "zstd" {
			t.Errorf("Content-Encoding = %q", r.Header.Get("Content-Encoding"))
		}
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"Text/Event-Stream; charset=utf-8"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{}}\n\n")),
		}, nil
	})
	srv := newSubscriptionTestServer(t, "", "", rt, nil)

	req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses/compact", bytes.NewReader([]byte{0x28, 0xb5, 0x2f, 0xfd}))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Encoding", "zstd")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	<-started
	snap := srv.live.snapshot()
	if len(snap.Requests) != 1 || snap.Requests[0].StreamState != "unknown" {
		t.Fatalf("live stream state = %+v, want unknown", snap.Requests)
	}
	close(release)
	<-done
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("response was not treated as SSE: status=%d headers=%v", rec.Code, rec.Header())
	}
}

func TestSubscriptionSensitiveErrors(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		rt := subscriptionRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header: http.Header{
					"Location":   []string{"https://evil.example/token"},
					"Set-Cookie": []string{"secret=cookie"},
				},
				Body: io.NopCloser(strings.NewReader("redirect body secret")),
			}, nil
		})
		srv := newSubscriptionTestServer(t, "", "", rt, nil)
		req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" || rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("redirect response = status %d headers %v", rec.Code, rec.Header())
		}
	})

	t.Run("error body is returned but not logged", func(t *testing.T) {
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		rt := subscriptionRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"sensitive-error-body"}`)),
			}, nil
		})
		srv := newSubscriptionTestServer(t, "", "", rt, logger)
		req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "sensitive-error-body") {
			t.Fatalf("response = status %d body %q", rec.Code, rec.Body.String())
		}
		if strings.Contains(logs.String(), "sensitive-error-body") {
			t.Fatalf("error body leaked to logs: %s", logs.String())
		}
	})

	t.Run("oversized error body becomes fixed 502", func(t *testing.T) {
		rt := subscriptionRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", maxErrorBodySize+1))),
			}, nil
		})
		srv := newSubscriptionTestServer(t, "", "", rt, nil)
		req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), strings.Repeat("x", 32)) {
			t.Fatalf("response = status %d body %q", rec.Code, rec.Body.String())
		}
	})
}

func TestSubscriptionRejectsKnownOversizedRequestBeforeForwarding(t *testing.T) {
	calls := 0
	rt := subscriptionRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	})
	srv := newSubscriptionTestServer(t, "", "", rt, nil)
	req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses", strings.NewReader(`{}`))
	req.ContentLength = maxRequestBody + 1
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || calls != 0 {
		t.Fatalf("status=%d upstream calls=%d", rec.Code, calls)
	}
}

func TestSubscriptionStatsDoNotPersistSensitiveValues(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/stats.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(subscriptionConfig("", ""), st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.upstreams["codex-subscription"].client = upstream.NewCodexSubscription(
		upstream.WithCodexSubscriptionRoundTripper(subscriptionRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"body-secret"}`)),
			}, nil
		})),
	)

	req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses?access_token=query-secret", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer bearer-secret")
	req.Header.Set("ChatGPT-Account-Id", "account-secret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	var clientPath, upstreamURL, statError string
	if err := st.DB().QueryRow(`SELECT client_path, upstream_url, error FROM requests LIMIT 1`).Scan(
		&clientPath, &upstreamURL, &statError); err != nil {
		t.Fatal(err)
	}
	if clientPath != "/codex/v1/responses" || upstreamURL != "https://chatgpt.com/backend-api/codex/responses" || statError != "upstream_auth_failed" {
		t.Fatalf("persisted record = path %q upstream %q error %q", clientPath, upstreamURL, statError)
	}
	all := clientPath + upstreamURL + statError
	for _, secret := range []string{"query-secret", "bearer-secret", "account-secret", "body-secret"} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q persisted in stats", secret)
		}
	}
}

func TestSubscriptionContextCancellationReachesUpstream(t *testing.T) {
	started := make(chan struct{})
	seenCanceled := make(chan struct{})
	rt := subscriptionRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(seenCanceled)
		return nil, r.Context().Err()
	})
	srv := newSubscriptionTestServer(t, "", "", rt, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/codex/v1/responses", strings.NewReader(`{"stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()
	<-started
	cancel()
	<-seenCanceled
	<-done
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}
