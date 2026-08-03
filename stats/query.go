package stats

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// DailyUsage is one aggregated row: per day, per upstream model. It is shaped
// as flat JSON so Grafana's Infinity datasource can consume it as a table
// without any transformation.
type DailyUsage struct {
	Day           string  `json:"day"`
	UpstreamModel string  `json:"upstream_model"`
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CachedTokens  int64   `json:"cached_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	MaxDurationMs int64   `json:"max_duration_ms"`
	AvgTTFTMs     float64 `json:"avg_ttft_ms"`
	MaxTTFTMs     int64   `json:"max_ttft_ms"`
}

// dailyUsageSQL groups requests by calendar day and upstream model. The day
// boundary honours tzOffset (first ? = a SQLite datetime modifier like
// "+8 hours"), so "per day" matches the viewer's local time, not UTC. The
// time column is RFC3339Nano UTC, whose lexical order equals chronological
// order, so the from/to bounds can be plain string comparisons.
const dailyUsageSQL = `
SELECT
    date(time, ?)                                       AS day,
    upstream_model,
    COUNT(*)                                            AS requests,
    SUM(CASE WHEN error IS NOT NULL THEN 1 ELSE 0 END)  AS errors,
    SUM(input_tokens)                                   AS input_tokens,
    SUM(output_tokens)                                  AS output_tokens,
    SUM(cached_tokens)                                  AS cached_tokens,
    SUM(total_tokens)                                   AS total_tokens,
    AVG(duration_ms)                                    AS avg_duration_ms,
    MAX(duration_ms)                                    AS max_duration_ms,
    COALESCE(AVG(ttft_ms), 0)                           AS avg_ttft_ms,
    COALESCE(MAX(ttft_ms), 0)                           AS max_ttft_ms
FROM requests
WHERE time >= ? AND time < ?
GROUP BY day, upstream_model
ORDER BY day DESC, total_tokens DESC`

// QueryDailyUsage returns per-day, per-upstream-model usage between [from, to).
// A zero from/to means unbounded on that side. tzOffsetHours shifts the day
// boundary away from UTC (e.g. 8 for UTC+8). The result is never nil so the
// JSON handler always emits a valid array.
func QueryDailyUsage(db *sql.DB, from, to time.Time, tzOffsetHours int) ([]DailyUsage, error) {
	lo := ""
	if !from.IsZero() {
		lo = from.UTC().Format(time.RFC3339Nano)
	}
	hi := "9999"
	if !to.IsZero() {
		hi = to.UTC().Format(time.RFC3339Nano)
	}
	modifier := fmt.Sprintf("%+d hours", tzOffsetHours)

	rows, err := db.Query(dailyUsageSQL, modifier, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DailyUsage, 0)
	for rows.Next() {
		var u DailyUsage
		if err := rows.Scan(
			&u.Day, &u.UpstreamModel,
			&u.Requests, &u.Errors,
			&u.InputTokens, &u.OutputTokens, &u.CachedTokens, &u.TotalTokens,
			&u.AvgDurationMs, &u.MaxDurationMs,
			&u.AvgTTFTMs, &u.MaxTTFTMs,
		); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- Interactive dashboard queries ----
//
// These power the /stats UI: a shared Filter narrows the rows, the same metric
// expression is reused across summary / per-model / time-series so every view
// reports identical numbers, and the detail list backs the request table.

// Filter narrows the request rows a query operates on. Zero From/To means
// unbounded on that side; empty Models/Protocols means no restriction on that
// dimension. TZOffsetHours shifts day/hour bucket boundaries away from UTC.
type Filter struct {
	From, To      time.Time
	Models        []string // matched against upstream_model
	Protocols     []string // matched against client_protocol
	TZOffsetHours int
}

// where builds the shared WHERE clause and its positional args. It always starts
// with " WHERE 1=1" so callers can append GROUP BY / ORDER BY without caring
// whether any predicate was added.
func (f Filter) where() (string, []any) {
	var b strings.Builder
	b.WriteString(" WHERE 1=1")
	var args []any
	if !f.From.IsZero() {
		b.WriteString(" AND time >= ?")
		args = append(args, f.From.UTC().Format(time.RFC3339Nano))
	}
	if !f.To.IsZero() {
		b.WriteString(" AND time < ?")
		args = append(args, f.To.UTC().Format(time.RFC3339Nano))
	}
	if len(f.Models) > 0 {
		b.WriteString(" AND upstream_model IN (")
		b.WriteString(placeholders(len(f.Models)))
		b.WriteString(")")
		for _, m := range f.Models {
			args = append(args, m)
		}
	}
	if len(f.Protocols) > 0 {
		b.WriteString(" AND client_protocol IN (")
		b.WriteString(placeholders(len(f.Protocols)))
		b.WriteString(")")
		for _, p := range f.Protocols {
			args = append(args, p)
		}
	}
	return b.String(), args
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// metricCols is the shared aggregate projection. Raw sums (duration, ttft) are
// scanned out so derived rates can be computed once in finalize, avoiding NULLIF
// division clutter in SQL. Column order must match scanMetrics.
const metricCols = `
    COUNT(*),
    COALESCE(SUM(CASE WHEN error IS NOT NULL AND error != '' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(stream), 0),
    COALESCE(SUM(input_tokens), 0),
    COALESCE(SUM(output_tokens), 0),
    COALESCE(SUM(cached_tokens), 0),
    COALESCE(SUM(total_tokens), 0),
    COALESCE(SUM(duration_ms), 0),
    COALESCE(AVG(duration_ms), 0),
    COALESCE(MAX(duration_ms), 0),
    COALESCE(SUM(COALESCE(ttft_ms, 0)), 0),
    COALESCE(AVG(ttft_ms), 0),
    COALESCE(MAX(ttft_ms), 0)`

// Metrics is the aggregate set every dashboard view reports. Token throughput
// follows the same convention as scripts/apid-stats.sql: e2e divides by total
// duration (perceived speed); post-TTFT subtracts time-to-first-token (can read
// high for heavy-reasoning requests, so treat as advisory).
type Metrics struct {
	Requests          int64   `json:"requests"`
	Errors            int64   `json:"errors"`
	StreamRequests    int64   `json:"stream_requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedTokens      int64   `json:"cached_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	AvgDurationMs     float64 `json:"avg_duration_ms"`
	MaxDurationMs     int64   `json:"max_duration_ms"`
	AvgTTFTMs         float64 `json:"avg_ttft_ms"`
	MaxTTFTMs         int64   `json:"max_ttft_ms"`
	CachePct          float64 `json:"cache_pct"`
	ErrorPct          float64 `json:"error_pct"`
	E2ETokPerSec      float64 `json:"e2e_tok_per_sec"`
	PostTTFTTokPerSec float64 `json:"post_ttft_tok_per_sec"`
}

// scanMetrics scans the metricCols projection (optionally preceded by group-key
// columns in leading) and computes the derived rates.
func scanMetrics(scan func(...any) error, leading ...any) (Metrics, error) {
	var m Metrics
	var sumDur, sumTTFT int64
	dest := append(leading,
		&m.Requests, &m.Errors, &m.StreamRequests,
		&m.InputTokens, &m.OutputTokens, &m.CachedTokens, &m.TotalTokens,
		&sumDur, &m.AvgDurationMs, &m.MaxDurationMs,
		&sumTTFT, &m.AvgTTFTMs, &m.MaxTTFTMs,
	)
	if err := scan(dest...); err != nil {
		return Metrics{}, err
	}
	m.finalize(sumDur, sumTTFT)
	return m, nil
}

func (m *Metrics) finalize(sumDurationMs, sumTTFTMs int64) {
	if m.InputTokens > 0 {
		m.CachePct = round2(100 * float64(m.CachedTokens) / float64(m.InputTokens))
	}
	if m.Requests > 0 {
		m.ErrorPct = round2(100 * float64(m.Errors) / float64(m.Requests))
	}
	if sumDurationMs > 0 {
		m.E2ETokPerSec = round1(1000 * float64(m.OutputTokens) / float64(sumDurationMs))
	}
	if d := sumDurationMs - sumTTFTMs; d > 0 {
		m.PostTTFTTokPerSec = round1(1000 * float64(m.OutputTokens) / float64(d))
	}
	m.AvgDurationMs = round1(m.AvgDurationMs)
	m.AvgTTFTMs = round1(m.AvgTTFTMs)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// QuerySummary returns the grand-total metrics for the filtered window.
func QuerySummary(db *sql.DB, f Filter) (Metrics, error) {
	where, args := f.where()
	q := "SELECT " + metricCols + " FROM requests" + where
	return scanMetrics(db.QueryRow(q, args...).Scan)
}

// ModelMetrics is one per-upstream-model aggregate row.
type ModelMetrics struct {
	UpstreamModel string `json:"upstream_model"`
	Metrics
}

// QueryByModel aggregates the filtered window per upstream model, busiest first.
func QueryByModel(db *sql.DB, f Filter) ([]ModelMetrics, error) {
	where, args := f.where()
	q := "SELECT upstream_model, " + metricCols + " FROM requests" + where +
		" GROUP BY upstream_model ORDER BY SUM(total_tokens) DESC, COUNT(*) DESC"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ModelMetrics, 0)
	for rows.Next() {
		var mm ModelMetrics
		m, err := scanMetrics(rows.Scan, &mm.UpstreamModel)
		if err != nil {
			return nil, err
		}
		mm.Metrics = m
		out = append(out, mm)
	}
	return out, rows.Err()
}

// TimeBucket is one time-bucketed aggregate row. Bucket is "YYYY-MM-DD" for day
// granularity or "YYYY-MM-DDTHH:MM" for any sub-day width (minutes floored onto
// the bucket grid; hour ends in ":00"), both in the viewer's local time.
type TimeBucket struct {
	Bucket string `json:"bucket"`
	Metrics
}

// bucketMinutes maps a time-series bucket unit to its width in minutes. 0 means a
// calendar day (date(), so month/DST boundaries stay correct). Unknown units fall
// back to day. Add a unit here and the SQL below handles it generically.
var bucketMinutes = map[string]int{
	"15min": 15,
	"hour":  60,
	"day":   0,
}

// QueryTimeSeries buckets the filtered window by unit (see bucketMinutes; default
// day), oldest first so it plots left-to-right. Sub-day widths floor each
// timestamp onto the local-time grid; only non-empty buckets are emitted, so the
// row count is bounded by the data, not the window width.
func QueryTimeSeries(db *sql.DB, f Filter, unit string) ([]TimeBucket, error) {
	modifier := fmt.Sprintf("%+d hours", f.TZOffsetHours)
	width := bucketMinutes[unit] // 0 (day) for unknown units

	bucketExpr := "date(time, ?)"
	modArgs := []any{modifier}
	if width > 0 {
		// Shift to local time, then subtract (localMinute % width) minutes to floor
		// onto the width grid; strftime truncates to the minute so seconds drop out.
		bucketExpr = fmt.Sprintf(
			"strftime('%%Y-%%m-%%dT%%H:%%M', time, ?, '-' || (CAST(strftime('%%M', time, ?) AS INTEGER) %% %d) || ' minutes')",
			width)
		modArgs = []any{modifier, modifier}
	}

	where, args := f.where()
	// The bucket placeholders sit in SELECT, before any WHERE placeholders.
	q := "SELECT " + bucketExpr + " AS bucket, " + metricCols + " FROM requests" +
		where + " GROUP BY bucket ORDER BY bucket ASC"
	allArgs := append(modArgs, args...)

	rows, err := db.Query(q, allArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TimeBucket, 0)
	for rows.Next() {
		var tb TimeBucket
		m, err := scanMetrics(rows.Scan, &tb.Bucket)
		if err != nil {
			return nil, err
		}
		tb.Metrics = m
		out = append(out, tb)
	}
	return out, rows.Err()
}

// RequestRow is one request detail row for the table. TTFTMs is nil when not
// measured (non-stream / failed / no content), matching the NULL in storage.
type RequestRow struct {
	Time           string `json:"time"`
	DurationMs     int64  `json:"duration_ms"`
	TTFTMs         *int64 `json:"ttft_ms"`
	ClientProtocol string `json:"client_protocol"`
	ClientModel    string `json:"client_model"`
	UpstreamModel  string `json:"upstream_model"`
	UpstreamURL    string `json:"upstream_url"`
	ClientUA       string `json:"client_ua"`
	SessionID      string `json:"session_id,omitempty"`
	Stream         bool   `json:"stream"`
	ClientStatus   int    `json:"client_status"`
	UpstreamStatus int    `json:"upstream_status"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	CachedTokens   int64  `json:"cached_tokens"`
	TotalTokens    int64  `json:"total_tokens"`
	Error          string `json:"error"`
}

// QueryRequests returns the newest matching request rows. errorsOnly restricts
// to failed requests. limit is clamped to [1, 1000] (default 100).
func QueryRequests(db *sql.DB, f Filter, errorsOnly bool, limit, offset int) ([]RequestRow, error) {
	where, args := f.where()
	if errorsOnly {
		where += " AND error IS NOT NULL AND error != ''"
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	q := `SELECT time, duration_ms, ttft_ms, client_protocol, client_model,
		upstream_model, upstream_url, client_ua, COALESCE(session_id, ''), stream, client_status,
        upstream_status, input_tokens, output_tokens, cached_tokens,
        total_tokens, COALESCE(error, '') FROM requests` + where +
		" ORDER BY time DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]RequestRow, 0)
	for rows.Next() {
		var (
			r      RequestRow
			ttft   sql.NullInt64
			stream int
		)
		if err := rows.Scan(
			&r.Time, &r.DurationMs, &ttft, &r.ClientProtocol, &r.ClientModel,
			&r.UpstreamModel, &r.UpstreamURL, &r.ClientUA, &r.SessionID, &stream, &r.ClientStatus,
			&r.UpstreamStatus, &r.InputTokens, &r.OutputTokens, &r.CachedTokens,
			&r.TotalTokens, &r.Error,
		); err != nil {
			return nil, err
		}
		if ttft.Valid {
			v := ttft.Int64
			r.TTFTMs = &v
		}
		r.Stream = stream == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// Options lists the distinct values used to populate the dashboard filter
// controls, plus the overall time span so the UI can default its range picker.
type Options struct {
	Models    []string `json:"models"`
	Protocols []string `json:"protocols"`
	MinTime   string   `json:"min_time"`
	MaxTime   string   `json:"max_time"`
}

// QueryOptions returns the distinct upstream models and client protocols seen so
// far, plus the min/max request time.
func QueryOptions(db *sql.DB) (Options, error) {
	o := Options{Models: []string{}, Protocols: []string{}}
	if err := collectDistinct(db, "upstream_model", &o.Models); err != nil {
		return o, err
	}
	if err := collectDistinct(db, "client_protocol", &o.Protocols); err != nil {
		return o, err
	}
	// Errors here are non-fatal: an empty table simply leaves the span blank.
	_ = db.QueryRow(`SELECT COALESCE(MIN(time), ''), COALESCE(MAX(time), '') FROM requests`).
		Scan(&o.MinTime, &o.MaxTime)
	return o, nil
}

// collectDistinct fills dst with the distinct non-empty values of a column.
// col is a trusted constant (not user input), so interpolation is safe here.
func collectDistinct(db *sql.DB, col string, dst *[]string) error {
	rows, err := db.Query("SELECT DISTINCT " + col + " FROM requests WHERE " + col + " != '' ORDER BY " + col)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		*dst = append(*dst, v)
	}
	return rows.Err()
}
