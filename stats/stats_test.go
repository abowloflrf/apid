package stats

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/abowloflrf/apid/store"
)

func TestRecorderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r := NewRecorder(st, 16, nil)
	if r == nil {
		t.Fatal("expected non-nil recorder")
	}
	defer r.Close()

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	records := []Record{
		{
			Time: now, Duration: 100 * time.Millisecond,
			ClientProtocol: "openai_responses",
			ClientPath:     "/v1/responses", ClientModel: "gpt-x",
			ClientStatus: 200, Stream: false,
			UpstreamProtocol: "openai_chat_completions",
			UpstreamURL:      "https://api.example.com/v1/chat/completions",
			UpstreamModel:    "real", UpstreamStatus: 200,
			Usage: &Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8, CachedTokens: 2},
		},
		{
			Time: now.Add(time.Second), Duration: 200 * time.Millisecond,
			ClientProtocol: "openai_responses",
			ClientPath:     "/v1/responses", ClientModel: "gpt-y",
			ClientStatus: 502, Stream: true,
			UpstreamProtocol: "openai_chat_completions",
			UpstreamURL:      "https://api.example.com/v1/chat/completions",
			UpstreamModel:    "real", UpstreamStatus: 502,
			Error: "upstream 502",
		},
	}
	for _, rec := range records {
		r.Record(rec)
	}
	r.Close() // 排空 channel

	rows, err := st.DB().Query(`SELECT
		time, duration_ms,
		client_protocol, client_path, client_model,
		upstream_protocol, upstream_url, upstream_model,
		stream, client_status, upstream_status,
		input_tokens, output_tokens, total_tokens, cached_tokens,
		IFNULL(error, '')
		FROM requests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var (
			timeStr, cPath, cModel, upURL, upModel, cProto, upProto, errStr         string
			duration, cStatus, upStatus, stream, inTok, outTok, totalTok, cachedTok int
		)
		if err := rows.Scan(&timeStr, &duration,
			&cProto, &cPath, &cModel,
			&upProto, &upURL, &upModel,
			&stream, &cStatus, &upStatus,
			&inTok, &outTok, &totalTok, &cachedTok,
			&errStr); err != nil {
			t.Fatal(err)
		}
		if cProto != "openai_responses" {
			t.Errorf("client_protocol = %q, want openai_responses", cProto)
		}
		if upProto != "openai_chat_completions" {
			t.Errorf("upstream_protocol = %q, want openai_chat_completions", upProto)
		}
		count++
	}
	if count != 2 {
		t.Errorf("read back %d rows, want 2", count)
	}
}

func TestRecorderNilSafe(t *testing.T) {
	// nil 接收者的所有方法都不应 panic，调用方可以放心 nil-check。
	var r *Recorder
	r.Record(Record{})
	r.Close()
	r.Close() // 重复 Close 也安全
}

func TestRecorderDisabled(t *testing.T) {
	// store 未启用时 NewRecorder 返回 nil。
	var st *store.Store
	if r := NewRecorder(st, 0, nil); r != nil {
		t.Errorf("expected nil recorder for nil store")
	}
	enabled, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	if r := NewRecorder(enabled, 0, nil); r != nil {
		t.Errorf("expected nil recorder for disabled store, got %+v", r)
	}
}

func TestRecorderEmptyError(t *testing.T) {
	// 无 error 时存 NULL；有 error 时存原文。
	path := filepath.Join(t.TempDir(), "stats.db")
	st, _ := store.Open(path)
	defer st.Close()
	r := NewRecorder(st, 0, nil)
	r.Record(Record{
		Time: time.Now(), ClientProtocol: "openai_responses", ClientPath: "/x", ClientModel: "m",
		ClientStatus: 200, UpstreamProtocol: "openai_chat_completions", UpstreamURL: "u", UpstreamModel: "m",
	})
	r.Record(Record{
		Time: time.Now(), ClientProtocol: "openai_responses", ClientPath: "/y", ClientModel: "m",
		ClientStatus: 500, UpstreamProtocol: "openai_chat_completions", UpstreamURL: "u", UpstreamModel: "m",
		Error: "boom",
	})
	r.Close()

	var nullCount, strCount int
	rows, _ := st.DB().Query("SELECT error FROM requests ORDER BY id")
	defer rows.Close()
	for rows.Next() {
		var e *string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		if e == nil {
			nullCount++
		} else {
			strCount++
		}
	}
	if nullCount != 1 || strCount != 1 {
		t.Errorf("nullCount=%d strCount=%d, want 1/1", nullCount, strCount)
	}
}
