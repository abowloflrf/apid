package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/stats"
	"github.com/abowloflrf/apid/store"
)

func TestHandleStatsDaily(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r := stats.NewRecorder(st, 16)
	r.Record(stats.Record{
		Time: time.Now().UTC(), Duration: 120 * time.Millisecond, TTFT: 30 * time.Millisecond,
		ClientProtocol: "openai_chat_completions", ClientPath: "/c", ClientModel: "gpt", ClientStatus: 200,
		UpstreamProtocol: "openai_chat_completions", UpstreamURL: "u", UpstreamModel: "gpt-4o", UpstreamStatus: 200,
		Usage: &stats.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CachedTokens: 20},
	})
	r.Close()

	srv := New(config.Config{}, st)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/stats/daily?tz_offset=8", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var rows []stats.DailyUsage
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, w.Body.String())
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].UpstreamModel != "gpt-4o" || rows[0].InputTokens != 100 || rows[0].CachedTokens != 20 {
		t.Errorf("unexpected row: %+v", rows[0])
	}
}

func TestHandleStatsDailyDisabled(t *testing.T) {
	// Storage off (nil store) -> 503, never panics.
	srv := New(config.Config{}, nil)
	req := httptest.NewRequest("GET", "/stats/daily", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandleStatsDailyBadParam(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, _ := store.Open(path)
	defer st.Close()
	srv := New(config.Config{}, st)

	req := httptest.NewRequest("GET", "/stats/daily?tz_offset=abc", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestClientAPIKeyExemptsHealthAndStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(config.Config{ClientAPIKey: "apid-key"}, st)
	h := srv.Handler()

	// With client auth on, the health probe and read-only stats/dashboard
	// endpoints stay open (no key) so the embedded dashboard and Grafana work.
	for _, path := range []string{"/healthz", "/stats/daily", "/stats/summary", "/stats/"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("GET %s should bypass client auth, got 401", path)
		}
	}
}
