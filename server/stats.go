package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abowloflrf/apid/stats"
)

// webuiFS holds the self-contained dashboard (HTML/CSS/JS + Chart.js under
// lib/), served read-only at /stats/. Embedding keeps apid a single binary with
// no external CDN dependency, so the UI works air-gapped.
//
//go:embed webui/dist
var webuiFS embed.FS

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
	s.writeJSON(w, rows)
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

// ---- Interactive dashboard API ----
//
// Endpoints under /stats/* power the embedded UI. All accept the same filter
// params (from/to/tz_offset/model/protocol); model and protocol may be repeated
// or comma-separated. They share parseFilter and writeJSON, and 503 when storage
// is off, mirroring handleStatsDaily.

// parseFilter builds a stats.Filter from the request query, validating bounds.
func parseFilter(r *http.Request) (stats.Filter, error) {
	q := r.URL.Query()
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		return stats.Filter{}, err
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		return stats.Filter{}, err
	}
	tz := 0
	if v := q.Get("tz_offset"); v != "" {
		if tz, err = strconv.Atoi(v); err != nil {
			return stats.Filter{}, err
		}
	}
	return stats.Filter{
		From:          from,
		To:            to,
		Models:        multiParam(q["model"]),
		Protocols:     multiParam(q["protocol"]),
		TZOffsetHours: tz,
	}, nil
}

// multiParam flattens repeated and comma-separated query values into a deduped,
// trimmed slice. Returns nil (no restriction) when nothing usable is present.
func multiParam(vals []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, v := range vals {
		for part := range strings.SplitSeq(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, dup := seen[part]; dup {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	return out
}

func (s *Server) handleStatsSummary(w http.ResponseWriter, r *http.Request) {
	f, ok := s.statsFilter(w, r)
	if !ok {
		return
	}
	m, err := stats.QuerySummary(s.db, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	s.writeJSON(w, m)
}

func (s *Server) handleStatsByModel(w http.ResponseWriter, r *http.Request) {
	f, ok := s.statsFilter(w, r)
	if !ok {
		return
	}
	rows, err := stats.QueryByModel(s.db, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	s.writeJSON(w, rows)
}

func (s *Server) handleStatsTimeSeries(w http.ResponseWriter, r *http.Request) {
	f, ok := s.statsFilter(w, r)
	if !ok {
		return
	}
	rows, err := stats.QueryTimeSeries(s.db, f, r.URL.Query().Get("bucket"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	s.writeJSON(w, rows)
}

func (s *Server) handleStatsRequests(w http.ResponseWriter, r *http.Request) {
	f, ok := s.statsFilter(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	errorsOnly := isTruthy(q.Get("errors_only"))
	rows, err := stats.QueryRequests(s.db, f, errorsOnly, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	s.writeJSON(w, rows)
}

func (s *Server) handleStatsOptions(w http.ResponseWriter, _ *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "stats storage not enabled (set APID_DB)")
		return
	}
	opts, err := stats.QueryOptions(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	s.writeJSON(w, opts)
}

// statsFilter parses the shared filter and guards on storage being enabled,
// writing the appropriate error itself. ok == false means a response was sent.
func (s *Server) statsFilter(w http.ResponseWriter, r *http.Request) (stats.Filter, bool) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "stats storage not enabled (set APID_DB)")
		return stats.Filter{}, false
	}
	f, err := parseFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid filter: "+err.Error())
		return stats.Filter{}, false
	}
	return f, true
}

// statsUIHandler serves the embedded dashboard at /stats/. It 503s when storage
// is off so the UI isn't reachable without data behind it.
func (s *Server) statsUIHandler() http.Handler {
	sub, err := fs.Sub(webuiFS, "webui/dist")
	if err != nil {
		// Embed paths are compile-time constants, so this never fails in practice.
		s.log.Error("stats webui sub-fs failed", "err", err)
		sub = webuiFS
	}
	fileServer := http.StripPrefix("/stats/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.db == nil {
			writeError(w, http.StatusServiceUnavailable, "stats storage not enabled (set APID_DB)")
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Warn("write response failed", "err", err)
	}
}
