package stats

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/abowloflrf/apid/store"
)

// seedDashboard inserts a small, deterministic dataset: model "a" has two
// streaming successes on day1, model "b" one failed sync request on day2.
func seedDashboard(t *testing.T) *sql.DB {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	r := NewRecorder(st, 16)
	day1 := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2025, 3, 2, 10, 0, 0, 0, time.UTC)

	rec1 := Record{
		Time: day1, ClientProtocol: "openai_chat_completions", ClientPath: "/c",
		ClientModel: "m", ClientStatus: 200, ClientUA: "ua-1",
		UpstreamProtocol: "openai_chat_completions", UpstreamURL: "u", UpstreamModel: "a",
		UpstreamStatus: 200, Stream: true,
		Duration: 100 * time.Millisecond, TTFT: 20 * time.Millisecond,
		Usage: &Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedTokens: 4},
	}
	rec2 := rec1
	rec2.Duration, rec2.TTFT = 300*time.Millisecond, 60*time.Millisecond
	rec2.Usage = &Usage{InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CachedTokens: 0}

	rec3 := Record{
		Time: day2, ClientProtocol: "anthropic_messages", ClientPath: "/m",
		ClientModel: "m", ClientStatus: 502, ClientUA: "ua-2",
		UpstreamProtocol: "anthropic_messages", UpstreamURL: "u", UpstreamModel: "b",
		UpstreamStatus: 502, Stream: false,
		Duration: 200 * time.Millisecond, Error: "boom",
	}

	for _, rec := range []Record{rec1, rec2, rec3} {
		r.Record(rec)
	}
	r.Close()
	return st.DB()
}

func TestQuerySummary(t *testing.T) {
	db := seedDashboard(t)
	s, err := QuerySummary(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests != 3 || s.Errors != 1 || s.StreamRequests != 2 {
		t.Errorf("requests=%d errors=%d stream=%d, want 3/1/2", s.Requests, s.Errors, s.StreamRequests)
	}
	if s.InputTokens != 30 || s.OutputTokens != 10 || s.TotalTokens != 40 || s.CachedTokens != 4 {
		t.Errorf("tokens in=%d out=%d total=%d cached=%d, want 30/10/40/4",
			s.InputTokens, s.OutputTokens, s.TotalTokens, s.CachedTokens)
	}
	if s.AvgDurationMs != 200 || s.MaxDurationMs != 300 {
		t.Errorf("duration avg=%v max=%d, want 200/300", s.AvgDurationMs, s.MaxDurationMs)
	}
	// AVG(ttft) ignores the NULL from the failed request -> (20+60)/2.
	if s.AvgTTFTMs != 40 || s.MaxTTFTMs != 60 {
		t.Errorf("ttft avg=%v max=%d, want 40/60", s.AvgTTFTMs, s.MaxTTFTMs)
	}
	if s.CachePct != 13.33 || s.ErrorPct != 33.33 {
		t.Errorf("cache_pct=%v error_pct=%v, want 13.33/33.33", s.CachePct, s.ErrorPct)
	}
	// e2e = 1000*10/600; post-TTFT = 1000*10/(600-80).
	if s.E2ETokPerSec != 16.7 || s.PostTTFTTokPerSec != 19.2 {
		t.Errorf("e2e=%v post=%v, want 16.7/19.2", s.E2ETokPerSec, s.PostTTFTTokPerSec)
	}
}

func TestQuerySummaryFilters(t *testing.T) {
	db := seedDashboard(t)

	byModel, err := QuerySummary(db, Filter{Models: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if byModel.Requests != 2 || byModel.Errors != 0 {
		t.Errorf("model a: requests=%d errors=%d, want 2/0", byModel.Requests, byModel.Errors)
	}

	byProto, err := QuerySummary(db, Filter{Protocols: []string{"anthropic_messages"}})
	if err != nil {
		t.Fatal(err)
	}
	if byProto.Requests != 1 || byProto.Errors != 1 {
		t.Errorf("anthropic: requests=%d errors=%d, want 1/1", byProto.Requests, byProto.Errors)
	}

	// Time bound that drops day1.
	bounded, err := QuerySummary(db, Filter{From: time.Date(2025, 3, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if bounded.Requests != 1 {
		t.Errorf("from-bounded requests=%d, want 1", bounded.Requests)
	}
}

// TestQuerySummaryEmpty guards the empty-window case: with no matching rows the
// aggregate SUMs are NULL in SQLite, so every column must COALESCE to 0 rather
// than fail the int64 scan.
func TestQuerySummaryEmpty(t *testing.T) {
	db := seedDashboard(t)
	// A window in the future matches nothing.
	s, err := QuerySummary(db, Filter{From: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("empty-window summary failed: %v", err)
	}
	if s.Requests != 0 || s.Errors != 0 || s.StreamRequests != 0 || s.TotalTokens != 0 {
		t.Errorf("empty window = %+v, want all-zero metrics", s)
	}
}

func TestQueryByModel(t *testing.T) {
	db := seedDashboard(t)
	rows, err := QueryByModel(db, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Ordered by total tokens DESC -> model a (40) before b (0).
	if rows[0].UpstreamModel != "a" || rows[0].Requests != 2 || rows[0].TotalTokens != 40 {
		t.Errorf("row0 = %+v, want model a / 2 reqs / 40 tokens", rows[0])
	}
	if rows[0].E2ETokPerSec != 25 { // 1000*10/400
		t.Errorf("model a e2e=%v, want 25", rows[0].E2ETokPerSec)
	}
	if rows[1].UpstreamModel != "b" || rows[1].Errors != 1 {
		t.Errorf("row1 = %+v, want model b with 1 error", rows[1])
	}
}

func TestQueryTimeSeries(t *testing.T) {
	db := seedDashboard(t)

	days, err := QueryTimeSeries(db, Filter{}, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d day buckets, want 2", len(days))
	}
	// Ascending order.
	if days[0].Bucket != "2025-03-01" || days[0].Requests != 2 {
		t.Errorf("bucket0 = %+v, want 2025-03-01 / 2 reqs", days[0])
	}
	if days[1].Bucket != "2025-03-02" || days[1].Requests != 1 {
		t.Errorf("bucket1 = %+v, want 2025-03-02 / 1 req", days[1])
	}

	// Hour granularity puts the two day1 requests in one HH:00 bucket.
	hours, err := QueryTimeSeries(db, Filter{Models: []string{"a"}}, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 1 || hours[0].Bucket != "2025-03-01T10:00" || hours[0].Requests != 2 {
		t.Fatalf("hour buckets = %+v, want single 2025-03-01T10:00 / 2 reqs", hours)
	}
}

func TestQueryTimeSeriesTZ(t *testing.T) {
	db := seedDashboard(t)
	// day2's request is at 10:00 UTC; UTC-12 shifts it back to the prior day.
	shifted, err := QueryTimeSeries(db, Filter{Models: []string{"b"}, TZOffsetHours: -12}, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(shifted) != 1 || shifted[0].Bucket != "2025-03-01" {
		t.Fatalf("tz-shifted bucket = %+v, want 2025-03-01", shifted)
	}
}

// TestQueryTimeSeriesSubHour checks sub-hour widths floor timestamps onto their
// grid: three requests at :02, :07 and :16 collapse differently per width.
func TestQueryTimeSeriesSubHour(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	r := NewRecorder(st, 16)
	base := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, min := range []int{2, 7, 16} {
		r.Record(Record{
			Time:             base.Add(time.Duration(min) * time.Minute),
			ClientProtocol:   "openai_chat_completions",
			UpstreamProtocol: "openai_chat_completions",
			UpstreamModel:    "a", ClientStatus: 200, UpstreamStatus: 200,
			Duration: 100 * time.Millisecond,
		})
	}
	r.Close()
	db := st.DB()

	cases := []struct {
		unit    string
		buckets []string
		counts  []int64
	}{
		{"15min", []string{"2025-03-01T10:00", "2025-03-01T10:15"}, []int64{2, 1}},
		{"hour", []string{"2025-03-01T10:00"}, []int64{3}},
	}
	for _, c := range cases {
		got, err := QueryTimeSeries(db, Filter{}, c.unit)
		if err != nil {
			t.Fatalf("%s: %v", c.unit, err)
		}
		if len(got) != len(c.buckets) {
			t.Fatalf("%s: got %d buckets %+v, want %d", c.unit, len(got), got, len(c.buckets))
		}
		for i := range got {
			if got[i].Bucket != c.buckets[i] || got[i].Requests != c.counts[i] {
				t.Errorf("%s bucket[%d] = %s/%d, want %s/%d",
					c.unit, i, got[i].Bucket, got[i].Requests, c.buckets[i], c.counts[i])
			}
		}
	}
}

func TestQueryRequests(t *testing.T) {
	db := seedDashboard(t)

	all, err := QueryRequests(db, Filter{}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d rows, want 3", len(all))
	}
	// Newest first: day2 (model b) leads.
	if all[0].UpstreamModel != "b" || all[0].Error != "boom" {
		t.Errorf("row0 = %+v, want model b with error", all[0])
	}
	if all[0].TTFTMs != nil {
		t.Errorf("failed request TTFTMs = %v, want nil", *all[0].TTFTMs)
	}
	// Stream rows carry a measured TTFT.
	var streamRow *RequestRow
	for i := range all {
		if all[i].Stream {
			streamRow = &all[i]
			break
		}
	}
	if streamRow == nil || streamRow.TTFTMs == nil {
		t.Fatal("expected a stream row with a non-nil TTFT")
	}

	errsOnly, err := QueryRequests(db, Filter{}, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(errsOnly) != 1 || errsOnly[0].UpstreamModel != "b" {
		t.Fatalf("errors-only = %+v, want single model b", errsOnly)
	}

	limited, err := QueryRequests(db, Filter{}, false, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Errorf("limited rows = %d, want 1", len(limited))
	}
}

func TestQueryOptions(t *testing.T) {
	db := seedDashboard(t)
	o, err := QueryOptions(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Models) != 2 || o.Models[0] != "a" || o.Models[1] != "b" {
		t.Errorf("models = %v, want [a b]", o.Models)
	}
	if len(o.Protocols) != 2 || o.Protocols[0] != "anthropic_messages" {
		t.Errorf("protocols = %v, want anthropic first", o.Protocols)
	}
	if o.MinTime == "" || o.MaxTime == "" || o.MinTime >= o.MaxTime {
		t.Errorf("time span = [%q,%q], want min<max", o.MinTime, o.MaxTime)
	}
}
