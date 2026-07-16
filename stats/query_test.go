package stats

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/abowloflrf/apid/store"
)

func TestQueryDailyUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r := NewRecorder(st, 16, nil)
	defer r.Close()

	// Two requests on day 1 for model "a", one on day 2 for model "b".
	day1 := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2025, 3, 2, 10, 0, 0, 0, time.UTC)
	base := Record{
		ClientProtocol: "openai_chat_completions", ClientPath: "/v1/chat/completions",
		ClientModel: "m", ClientStatus: 200,
		UpstreamProtocol: "openai_chat_completions", UpstreamURL: "u", UpstreamStatus: 200,
	}
	rec1 := base
	rec1.Time, rec1.UpstreamModel, rec1.Duration, rec1.TTFT = day1, "a", 100*time.Millisecond, 20*time.Millisecond
	rec1.Usage = &Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedTokens: 4}
	rec2 := base
	rec2.Time, rec2.UpstreamModel, rec2.Duration, rec2.TTFT = day1, "a", 300*time.Millisecond, 60*time.Millisecond
	rec2.Usage = &Usage{InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CachedTokens: 0}
	rec3 := base
	rec3.Time, rec3.UpstreamModel, rec3.Duration = day2, "b", 200*time.Millisecond
	rec3.ClientStatus, rec3.UpstreamStatus, rec3.Error = 502, 502, "boom"

	for _, rec := range []Record{rec1, rec2, rec3} {
		r.Record(rec)
	}
	r.Close()

	rows, err := QueryDailyUsage(st.DB(), time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	// Ordered by day DESC, so day2/model b comes first.
	byModel := map[string]DailyUsage{}
	for _, u := range rows {
		byModel[u.UpstreamModel] = u
	}

	a := byModel["a"]
	if a.Day != "2025-03-01" {
		t.Errorf("day = %q, want 2025-03-01", a.Day)
	}
	if a.Requests != 2 || a.Errors != 0 {
		t.Errorf("requests=%d errors=%d, want 2/0", a.Requests, a.Errors)
	}
	if a.InputTokens != 30 || a.OutputTokens != 10 || a.TotalTokens != 40 || a.CachedTokens != 4 {
		t.Errorf("tokens in=%d out=%d total=%d cached=%d, want 30/10/40/4",
			a.InputTokens, a.OutputTokens, a.TotalTokens, a.CachedTokens)
	}
	if a.AvgDurationMs != 200 || a.MaxDurationMs != 300 {
		t.Errorf("duration avg=%v max=%d, want 200/300", a.AvgDurationMs, a.MaxDurationMs)
	}
	if a.AvgTTFTMs != 40 || a.MaxTTFTMs != 60 {
		t.Errorf("ttft avg=%v max=%d, want 40/60", a.AvgTTFTMs, a.MaxTTFTMs)
	}

	b := byModel["b"]
	if b.Requests != 1 || b.Errors != 1 {
		t.Errorf("model b requests=%d errors=%d, want 1/1", b.Requests, b.Errors)
	}
	// No TTFT recorded for the failed request -> COALESCE to 0.
	if b.AvgTTFTMs != 0 || b.MaxTTFTMs != 0 {
		t.Errorf("model b ttft avg=%v max=%d, want 0/0", b.AvgTTFTMs, b.MaxTTFTMs)
	}
}

func TestQueryDailyUsageTimeRangeAndTZ(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, _ := store.Open(path)
	defer st.Close()
	r := NewRecorder(st, 16, nil)
	defer r.Close()

	base := Record{
		ClientProtocol: "openai_chat_completions", ClientPath: "/c", ClientModel: "m", ClientStatus: 200,
		UpstreamProtocol: "openai_chat_completions", UpstreamURL: "u", UpstreamModel: "a", UpstreamStatus: 200,
	}
	// 23:30 UTC on Mar 1 -> 07:30 on Mar 2 at UTC+8: the day boundary differs.
	rec := base
	rec.Time = time.Date(2025, 3, 1, 23, 30, 0, 0, time.UTC)
	r.Record(rec)
	r.Close()

	utc, err := QueryDailyUsage(st.DB(), time.Time{}, time.Time{}, 0)
	if err != nil || len(utc) != 1 || utc[0].Day != "2025-03-01" {
		t.Fatalf("utc day = %+v, want 2025-03-01", utc)
	}
	shifted, err := QueryDailyUsage(st.DB(), time.Time{}, time.Time{}, 8)
	if err != nil || len(shifted) != 1 || shifted[0].Day != "2025-03-02" {
		t.Fatalf("utc+8 day = %+v, want 2025-03-02", shifted)
	}

	// from bound excludes everything before Mar 2.
	none, err := QueryDailyUsage(st.DB(), time.Date(2025, 3, 2, 0, 0, 0, 0, time.UTC), time.Time{}, 0)
	if err != nil || len(none) != 0 {
		t.Fatalf("from-bounded rows = %+v, want empty", none)
	}
}
