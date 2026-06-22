package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/stats"
	"github.com/abowloflrf/apid/internal/store"
)

func seedServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	r := stats.NewRecorder(st, 16)
	now := time.Now().UTC()
	r.Record(stats.Record{
		Time: now, Duration: 200 * time.Millisecond, TTFT: 40 * time.Millisecond,
		ClientProtocol: "openai_responses", ClientPath: "/r", ClientModel: "m", ClientStatus: 200,
		UpstreamProtocol: "openai_chat_completions", UpstreamURL: "u", UpstreamModel: "gpt-x",
		UpstreamStatus: 200, Stream: true,
		Usage: &stats.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CachedTokens: 20},
	})
	r.Record(stats.Record{
		Time: now, Duration: 80 * time.Millisecond,
		ClientProtocol: "anthropic_messages", ClientPath: "/m", ClientModel: "m", ClientStatus: 502,
		UpstreamProtocol: "anthropic_messages", UpstreamURL: "u", UpstreamModel: "claude-x",
		UpstreamStatus: 502, Error: "boom",
	})
	r.Close()

	return New(config.Config{}, st)
}

func TestDashboardSummary(t *testing.T) {
	h := seedServer(t).Handler()
	req := httptest.NewRequest("GET", "/stats/summary", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var s stats.Metrics
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if s.Requests != 2 || s.Errors != 1 || s.TotalTokens != 150 {
		t.Errorf("summary = %+v, want 2 reqs / 1 err / 150 tokens", s)
	}
}

func TestDashboardSummaryModelFilter(t *testing.T) {
	h := seedServer(t).Handler()
	req := httptest.NewRequest("GET", "/stats/summary?model=gpt-x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var s stats.Metrics
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	if s.Requests != 1 || s.Errors != 0 {
		t.Errorf("filtered summary = %+v, want 1 req / 0 err", s)
	}
}

func TestDashboardOptions(t *testing.T) {
	h := seedServer(t).Handler()
	req := httptest.NewRequest("GET", "/stats/options", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var o stats.Options
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(o.Models) != 2 || len(o.Protocols) != 2 {
		t.Errorf("options = %+v, want 2 models / 2 protocols", o)
	}
}

func TestDashboardRequestsErrorsOnly(t *testing.T) {
	h := seedServer(t).Handler()
	req := httptest.NewRequest("GET", "/stats/requests?errors_only=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var rows []stats.RequestRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0].Error != "boom" {
		t.Fatalf("errors-only rows = %+v, want single boom", rows)
	}
}

// The specific API patterns must outrank the /stats/ file-server subtree.
func TestDashboardRoutingPrecedence(t *testing.T) {
	h := seedServer(t).Handler()

	api := httptest.NewRequest("GET", "/stats/summary", nil)
	aw := httptest.NewRecorder()
	h.ServeHTTP(aw, api)
	if ct := aw.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("/stats/summary content-type = %q, want json", ct)
	}

	page := httptest.NewRequest("GET", "/stats/", nil)
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, page)
	if pw.Code != http.StatusOK {
		t.Fatalf("/stats/ status = %d, want 200", pw.Code)
	}
	if !strings.Contains(pw.Body.String(), "apid") {
		t.Errorf("/stats/ did not serve the dashboard page")
	}

	asset := httptest.NewRequest("GET", "/stats/lib/chart.umd.min.js", nil)
	aw2 := httptest.NewRecorder()
	h.ServeHTTP(aw2, asset)
	if aw2.Code != http.StatusOK {
		t.Errorf("/stats/app.js status = %d, want 200", aw2.Code)
	}
}

func TestDashboardDisabled(t *testing.T) {
	srv := New(config.Config{}, nil) // storage off
	for _, p := range []string{"/stats/summary", "/stats/by_model", "/stats/timeseries", "/stats/requests", "/stats/options", "/stats/"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503", p, w.Code)
		}
	}
}
