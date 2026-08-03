package server

// GET /stats/sessions lists local AI coding agent sessions (Codex / Claude
// Code / pi / OpenCode) for the dashboard's sessions view. The data comes from
// the machine apid runs on — the agents' transcript stores, read-only — so,
// like /stats/topology and /stats/active, this endpoint needs no storage DB.
//
// Params (all optional):
//
//	tool         repeatable; codex | claude | pi | opencode (default: all)
//	q            substring filter over cwd and title
//	cwd          substring filter over cwd only
//	source       substring filter over source (Codex only)
//	since        unix ms, YYYY-MM-DD, ISO-8601, or relative like 7d/12h/30m
//	archived     0 (default) hides archived, 1 shows only archived, absent = both
//	sort         updated (default) | created
//	limit        page size (default 20, max 200); offset for paging
//	with_tokens  1 enriches the page with token usage / cache hit rate (slower:
//	             pi/claude need a full transcript pass, memoized per file)
//	sources      0 hides the data-source list
//
// Token columns for codex/opencode come straight from their databases; pi and
// claude only get them with with_tokens=1.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/abowloflrf/apid/session"
)

const (
	sessionsDefaultLimit = 20
	sessionsMaxLimit     = 200
)

type sessionsResponse struct {
	Sources    []session.SourceInfo `json:"sources"`
	Total      int                  `json:"total"`
	Limit      int                  `json:"limit"`
	Offset     int                  `json:"offset"`
	WithTokens bool                 `json:"with_tokens"`
	Sessions   []session.Session    `json:"sessions"`
	Summary    session.Summary      `json:"summary"`
	Generated  int64                `json:"generated_ms"`
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var tools []string
	for _, t := range q["tool"] {
		switch t {
		case session.ToolCodex, session.ToolClaude, session.ToolPi, session.ToolOpenCode:
			tools = append(tools, t)
		case "", "all":
			// no tool filter
		default:
			writeError(w, http.StatusBadRequest, "invalid tool: "+t)
			return
		}
	}

	var archived *bool
	if v := q.Get("archived"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid archived: "+v)
			return
		}
		archived = &b
	}

	limit := sessionsDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit: "+v)
			return
		}
		limit = min(n, sessionsMaxLimit)
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset: "+v)
			return
		}
		offset = n
	}

	since := int64(0)
	if v := q.Get("since"); v != "" {
		t, err := session.ParseSince(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		since = t
	}

	sortKey := q.Get("sort")
	if sortKey == "" {
		sortKey = "updated"
	}
	if sortKey != "updated" && sortKey != "created" {
		writeError(w, http.StatusBadRequest, "invalid sort: "+sortKey)
		return
	}

	withTokens := q.Get("with_tokens") == "1"
	showSources := q.Get("sources") != "0"

	res := s.sessions.List(session.Query{
		Tools:    tools,
		Archived: archived,
		Q:        q.Get("q"),
		CWD:      q.Get("cwd"),
		Source:   q.Get("source"),
		Since:    since,
		Sort:     sortKey,
		Limit:    limit,
		Offset:   offset,
	}, withTokens)

	out := sessionsResponse{
		Total:      res.Total,
		Limit:      limit,
		Offset:     offset,
		WithTokens: withTokens,
		Sessions:   res.Sessions,
		Summary:    session.Summarize(res.Sessions),
		Generated:  time.Now().UnixMilli(),
	}
	if showSources {
		out.Sources = res.Sources
	}
	s.writeJSON(w, out)
}
