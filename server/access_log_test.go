package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
)

func TestAccessLogCoversEveryRequest(t *testing.T) {
	tests := []struct {
		name          string
		cfg           config.Config
		method        string
		requestTarget string
		wantPath      string
		wantStatus    int
		wantLevel     string
	}{
		{
			name:          "success",
			method:        http.MethodGet,
			requestTarget: "/healthz",
			wantPath:      "/healthz",
			wantStatus:    http.StatusOK,
			wantLevel:     "INFO",
		},
		{
			name:          "unknown path",
			method:        http.MethodPost,
			requestTarget: "/wrong/path?access_token=do-not-log",
			wantPath:      "/wrong/path",
			wantStatus:    http.StatusNotFound,
			wantLevel:     "WARN",
		},
		{
			name: "wrong method",
			cfg: config.Config{Routes: []config.Route{{
				Path: "/v1/responses",
			}}},
			method:        http.MethodGet,
			requestTarget: "/v1/responses",
			wantPath:      "/v1/responses",
			wantStatus:    http.StatusMethodNotAllowed,
			wantLevel:     "WARN",
		},
		{
			name: "authentication failure",
			cfg: config.Config{
				ClientAPIKey: "secret",
				Routes:       []config.Route{{Path: "/v1/responses"}},
			},
			method:        http.MethodPost,
			requestTarget: "/v1/responses",
			wantPath:      "/v1/responses",
			wantStatus:    http.StatusUnauthorized,
			wantLevel:     "WARN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			srv := New(tt.cfg, nil, logger)
			defer srv.Close()

			req := httptest.NewRequest(tt.method, tt.requestTarget, nil)
			req.RemoteAddr = "203.0.113.9:43100"
			req.Header.Set("User-Agent", "apid-test/1.0")
			req.Header.Set("X-Forwarded-For", "198.51.100.7")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			got := logs.String()
			if count := strings.Count(got, "msg=access"); count != 1 {
				t.Fatalf("access log count = %d, want 1: %q", count, got)
			}
			for _, want := range []string{
				"level=" + tt.wantLevel,
				"msg=access",
				"method=" + tt.method,
				"path=" + tt.wantPath,
				"client_ip=203.0.113.9",
				"user_agent=apid-test/1.0",
				"status=" + strconv.Itoa(tt.wantStatus),
				"response_bytes=" + strconv.Itoa(rec.Body.Len()),
				"duration=",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("log %q does not contain %q", got, want)
				}
			}
			for _, sensitive := range []string{"do-not-log", "198.51.100.7"} {
				if strings.Contains(got, sensitive) {
					t.Errorf("untrusted or sensitive value %q leaked to log: %q", sensitive, got)
				}
			}
		})
	}
}

func TestForwardingAccessLogIsEnrichedAndNotDuplicated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer up.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	srv := New(config.Config{
		Upstreams: []config.Upstream{{
			Name: "chat", Protocol: config.ProtoChat, BaseURL: up.URL, Path: "/v1/chat/completions",
		}},
		Routes: []config.Route{{
			Path: "/v1/chat/completions", InputProtocol: config.ProtoChat,
			Models: []config.ModelRule{{Match: "*", Upstream: "chat"}},
		}},
	}, nil, logger)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?api_key=do-not-log", strings.NewReader(`{"model":"gpt-x"}`))
	req.RemoteAddr = "[2001:db8::1]:54321"
	req.Header.Set("User-Agent", "llm-client/2.0")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got := logs.String()
	if count := strings.Count(got, "msg=access"); count != 1 {
		t.Fatalf("access log count = %d, want 1: %q", count, got)
	}
	for _, want := range []string{
		"level=INFO", "method=POST", "path=/v1/chat/completions",
		"client_ip=2001:db8::1", "user_agent=llm-client/2.0", "status=200",
		"response_bytes=" + strconv.Itoa(rec.Body.Len()), "route=/v1/chat/completions",
		"client_protocol=openai_chat_completions", "model=gpt-x", "stream=false",
		"upstream_protocol=openai_chat_completions", "upstream_url=" + up.URL + "/v1/chat/completions",
		"upstream_model=gpt-x", "upstream_status=200",
		"usage.input=2", "usage.output=3", "usage.total=5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "do-not-log") {
		t.Errorf("query secret leaked to access log: %q", got)
	}
}

func TestAccessLogRecordsPanicsAndRepanics(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	srv := New(config.Config{}, nil, logger)
	defer srv.Close()
	handler := srv.withAccessLog(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	func() {
		defer func() {
			if recovered := recover(); recovered != "boom" {
				t.Fatalf("recovered = %v, want boom", recovered)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()

	got := logs.String()
	for _, want := range []string{"level=ERROR", "msg=access", "path=/panic", "status=500", "panic=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q does not contain %q", got, want)
		}
	}
}

func TestRespRecorderUsesFirstFinalStatusAndCountsBytes(t *testing.T) {
	underlying := httptest.NewRecorder()
	rec := &respRecorder{ResponseWriter: underlying}
	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusInternalServerError)
	n, err := rec.Write([]byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || rec.bytes != 3 {
		t.Fatalf("write = %d bytes, recorded = %d, want 3", n, rec.bytes)
	}
	if got := rec.statusCode(); got != http.StatusCreated {
		t.Fatalf("status = %d, want %d", got, http.StatusCreated)
	}
	if underlying.Code != http.StatusCreated {
		t.Fatalf("underlying status = %d, want %d", underlying.Code, http.StatusCreated)
	}
}

func TestRemoteIP(t *testing.T) {
	tests := map[string]string{
		"203.0.113.8:4321":      "203.0.113.8",
		"[2001:db8::8]:4321":    "2001:db8::8",
		"2001:0db8:0:0:0:0:0:8": "2001:db8::8",
		"client.example":        "client.example",
	}
	for input, want := range tests {
		if got := remoteIP(input); got != want {
			t.Errorf("remoteIP(%q) = %q, want %q", input, got, want)
		}
	}
}
