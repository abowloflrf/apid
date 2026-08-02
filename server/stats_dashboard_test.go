package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/stats"
	"github.com/abowloflrf/apid/store"
)

// assetRef matches the Vite-built page's relative asset references
// ("./assets/index-<hash>.js"), whose filenames change on every build.
var assetRef = regexp.MustCompile(`(?:src|href)="\./(assets/[^"]+)"`)

func seedServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	r := stats.NewRecorder(st, 16, nil)
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

	return New(config.Config{}, st, nil)
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
	if !webuiBuilt {
		// Frontend not built: the committed placeholder only makes the embed
		// compile; the UI is intentionally unavailable.
		if pw.Code != http.StatusServiceUnavailable {
			t.Fatalf("/stats/ status = %d, want 503 (webui not built)", pw.Code)
		}
		return
	}
	if pw.Code != http.StatusOK {
		t.Fatalf("/stats/ status = %d, want 200", pw.Code)
	}
	if !strings.Contains(pw.Body.String(), "apid") {
		t.Errorf("/stats/ did not serve the dashboard page")
	}

	// Vite emits hashed asset filenames (assets/index-*.js/css), so resolve one
	// from the served page instead of hard-coding a path.
	assetMatch := assetRef.FindStringSubmatch(pw.Body.String())
	if len(assetMatch) < 2 {
		return
	}
	asset := httptest.NewRequest("GET", "/stats/"+assetMatch[1], nil)
	aw2 := httptest.NewRecorder()
	h.ServeHTTP(aw2, asset)
	if aw2.Code != http.StatusOK {
		t.Errorf("/stats/%s status = %d, want 200", assetMatch[1], aw2.Code)
	}
}

// The /stats -> /stats/ redirect must be relative so it survives being mounted
// under a reverse-proxy subpath; an absolute "/stats/" would drop the prefix.
func TestStatsRedirectIsRelative(t *testing.T) {
	h := seedServer(t).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("/stats status = %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "stats/" {
		t.Errorf("Location = %q, want relative %q", loc, "stats/")
	}
}

func TestDashboardDisabled(t *testing.T) {
	srv := New(config.Config{}, nil, nil) // storage off
	for _, p := range []string{"/stats/summary", "/stats/by_model", "/stats/timeseries", "/stats/requests", "/stats/options", "/stats/"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503", p, w.Code)
		}
	}
}
