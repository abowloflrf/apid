package stats

import (
	"database/sql"
	"fmt"
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
