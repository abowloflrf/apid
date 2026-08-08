package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/abowloflrf/apid/config"
)

func TestHello(t *testing.T) {
	var upstreamCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	srv := New(config.Config{
		ClientAPIKey: "client-key",
		StatsAPIKey:  "stats-key",
		Upstreams: []config.Upstream{{
			Name:     "up",
			Protocol: config.ProtoChat,
			BaseURL:  up.URL,
			Path:     "/v1/chat/completions",
		}},
		Routes: []config.Route{{
			Path:          "/api/hello",
			InputProtocol: config.ProtoChat,
			Models:        []config.ModelRule{{Match: "*", Upstream: "up"}},
		}},
	}, nil, nil)
	t.Cleanup(srv.Close)

	tests := []struct {
		method   string
		wantBody string
	}{
		{method: http.MethodGet, wantBody: helloResponse},
		{method: http.MethodHead, wantBody: ""},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, "/api/hello", nil)
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s /api/hello status = %d, want 200", tt.method, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("%s /api/hello Content-Type = %q, want application/json", tt.method, got)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("%s /api/hello body = %q, want %q", tt.method, got, tt.wantBody)
			}
		})
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/hello", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/hello without API key status = %d, want 401", rec.Code)
	}

	if got := upstreamCalls.Load(); got != 0 {
		t.Errorf("/api/hello made %d upstream requests, want 0", got)
	}
}
