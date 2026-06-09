package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/abowloflrf/apid/internal/stats"
)

// handleStatsDaily serves per-day, per-upstream-model usage as a flat JSON
// array for Grafana's Infinity datasource. Query params (all optional):
//
//	from, to    time bounds; Unix milliseconds (Grafana ${__from}/${__to})
//	            or RFC3339. Range is [from, to).
//	tz_offset   hour offset for the day boundary, e.g. 8 for UTC+8 (default 0).
func (s *Server) handleStatsDaily(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "stats storage not enabled (set APID_DB)")
		return
	}
	q := r.URL.Query()
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid from: "+err.Error())
		return
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid to: "+err.Error())
		return
	}
	tzOffset := 0
	if v := q.Get("tz_offset"); v != "" {
		tzOffset, err = strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid tz_offset: "+err.Error())
			return
		}
	}

	rows, err := stats.QueryDailyUsage(s.db, from, to, tzOffset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rows); err != nil {
		log.Printf("stats: failed to write response: %v", err)
	}
}

// parseTimeParam accepts a Unix-millisecond timestamp (what Grafana's
// ${__from}/${__to} expand to) or an RFC3339 string. Empty returns the zero
// time, meaning "unbounded".
func parseTimeParam(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	return time.Parse(time.RFC3339, s)
}
