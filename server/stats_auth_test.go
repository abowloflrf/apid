package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
)

// TestStatsAPIKeyProtectsStatsEndpoints: stats_api_key 设置后，/stats(*) 必须携带
// 对应 key；client_api_key（转发路由用的）不能访问 stats，两者相互隔离。
func TestStatsAPIKeyProtectsStatsEndpoints(t *testing.T) {
	srv := New(config.Config{StatsAPIKey: "stats-key"}, nil, nil)
	h := srv.Handler()

	for _, path := range []string{"/stats/topology", "/stats/", "/stats"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without key = %d, want 401", path, w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s missing WWW-Authenticate header", path)
		}
	}

	// 转发路由的 client key 不能访问 stats。
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/stats/topology", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("client key on stats = %d, want 401", w.Code)
	}

	// 三种 header 风格均可。
	for _, setHeader := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer stats-key") },
		func(r *http.Request) { r.Header.Set("X-Api-Key", "stats-key") },
		func(r *http.Request) { r.Header.Set("Api-Key", "stats-key") },
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/stats/topology", nil)
		setHeader(req)
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("valid stats key status = %d, want 200 (body=%s)", w.Code, w.Body.String())
		}
	}
}

// TestStatsAPIKeyLeavesHealthOpen: /healthz 始终豁免。
func TestStatsAPIKeyLeavesHealthOpen(t *testing.T) {
	srv := New(config.Config{StatsAPIKey: "stats-key", ClientAPIKey: "client-key"}, nil, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", w.Code)
	}
}

// TestStatsAPIKeyDoesNotProtectForwardRoutes: 只设 stats_api_key 时，转发路由
// 不受影响（不要求认证）。
func TestStatsAPIKeyDoesNotProtectForwardRoutes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[],"usage":{}}`))
	}))
	defer up.Close()

	cfg := forwardConfig("/v1/chat/completions", config.ProtoChat, up.URL, "/v1/chat/completions", "")
	cfg.StatsAPIKey = "stats-key"
	srv := New(cfg, nil, nil)
	defer srv.Close()

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[]}`)))
	if w.Code == http.StatusUnauthorized {
		t.Errorf("POST forward route = 401, stats_api_key must not guard forwarding routes")
	}
}
