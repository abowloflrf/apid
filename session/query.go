package session

// Query layer on top of the readers: a Loader caches the full listing scan
// briefly (the scan touches every transcript file), then filters/sorts/pages
// per request. Enrichment only touches the requested page, never the cache.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Query carries the listing filters; zero values mean "no filter".
type Query struct {
	Tools    []string // empty = all tools
	Archived *bool    // nil = both states
	Q        string   // cwd/title substring, case-insensitive
	CWD      string   // cwd substring, case-insensitive
	Source   string   // source substring, case-insensitive (codex only)
	Since    int64    // unix ms; only sessions updated at/after this
	Sort     string   // "updated" (default) | "created"
	Limit    int      // page size; <= 0 means no limit
	Offset   int
}

// SourceInfo describes one backing store, for the "data source" line.
type SourceInfo struct {
	Tool  string `json:"tool"`
	Label string `json:"label"` // e.g. "Codex"
	Desc  string `json:"desc"`  // e.g. "state_5.sqlite"
}

// Result is one page of matching sessions plus totals and sources.
type Result struct {
	Sessions []Session    `json:"sessions"`
	Total    int          `json:"total"`
	Sources  []SourceInfo `json:"sources"`
}

// Loader caches the listing scan for ttl and enriches through a shared cache.
type Loader struct {
	mu      sync.Mutex
	ttl     time.Duration
	tools   []string
	loaded  time.Time
	sess    []Session
	sources []SourceInfo
	enrich  *EnrichCache
}

// NewLoader returns a Loader with a 60s listing cache.
func NewLoader() *Loader {
	return &Loader{ttl: 60 * time.Second, enrich: NewEnrichCache()}
}

// NewLoaderForTools returns a Loader that only scans the selected backing
// stores. It is useful for one-shot commands; the server uses NewLoader so its
// cache can serve arbitrary filters.
func NewLoaderForTools(tools ...string) *Loader {
	return &Loader{
		ttl:    60 * time.Second,
		tools:  append([]string(nil), tools...),
		enrich: NewEnrichCache(),
	}
}

// List returns the page of sessions matching q. When withTokens is set all
// matching sessions are enriched before paging, so zero-token sessions can be
// excluded from the result and its total. Without it only the cheap metadata
// columns are returned.
func (l *Loader) List(q Query, withTokens bool) Result {
	return l.list(q, withTokens, false)
}

// Display returns an enriched display page and stops scanning once that page
// is full. It mirrors the standalone script's fast path; callers that need an
// exact total across all token-bearing sessions should use List instead.
func (l *Loader) Display(q Query) Result {
	return l.list(q, true, true)
}

func (l *Loader) list(q Query, withTokens, stopAfterPage bool) Result {
	all, sources := l.scan()
	all = filterSessions(all, q)
	sortSessions(all, q.Sort)

	if withTokens {
		enriched := make([]Session, 0, len(all))
		for _, s := range all {
			l.enrich.Enrich(&s)
			if s.TokensUsed > 0 {
				enriched = append(enriched, s)
				if stopAfterPage && q.Limit > 0 && len(enriched) >= q.Offset+q.Limit {
					break
				}
			}
		}
		all = enriched
	}

	res := Result{Total: len(all), Sources: filterSources(sources, q.Tools)}
	start, end := pageBounds(q.Offset, q.Limit, len(all))
	res.Sessions = make([]Session, 0, end-start)
	for _, s := range all[start:end] {
		res.Sessions = append(res.Sessions, s) // value copy: enrich must not dirty the cache
	}
	return res
}

func filterSources(sources []SourceInfo, tools []string) []SourceInfo {
	if len(tools) == 0 {
		return sources
	}
	out := make([]SourceInfo, 0, len(sources))
	for _, source := range sources {
		if contains(tools, source.Tool) {
			out = append(out, source)
		}
	}
	return out
}

// scan lists every session once per ttl window.
func (l *Loader) scan() ([]Session, []SourceInfo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.loaded) < l.ttl && l.sess != nil {
		return l.sess, l.sources
	}
	var sess []Session
	var sources []SourceInfo

	if l.wantTool(ToolCodex) {
		s, desc := loadCodex()
		sess = append(sess, s...)
		if desc != "" {
			sources = append(sources, SourceInfo{Tool: ToolCodex, Label: "Codex", Desc: desc})
		}
	}
	if l.wantTool(ToolClaude) {
		s, desc := loadClaude()
		sess = append(sess, s...)
		if desc != "" {
			sources = append(sources, SourceInfo{Tool: ToolClaude, Label: "Claude", Desc: desc})
		}
	}
	if l.wantTool(ToolPi) {
		s, desc := loadPi()
		sess = append(sess, s...)
		if desc != "" {
			sources = append(sources, SourceInfo{Tool: ToolPi, Label: "pi", Desc: desc})
		}
	}
	if l.wantTool(ToolOpenCode) {
		s, desc := loadOpenCode()
		sess = append(sess, s...)
		if desc != "" {
			sources = append(sources, SourceInfo{Tool: ToolOpenCode, Label: "OpenCode", Desc: desc})
		}
	}

	l.sess, l.sources, l.loaded = sess, sources, time.Now()
	return sess, sources
}

func (l *Loader) wantTool(tool string) bool {
	return len(l.tools) == 0 || contains(l.tools, tool)
}

func filterSessions(all []Session, q Query) []Session {
	out := make([]Session, 0, len(all))
	needle := strings.ToLower(q.Q)
	cwdNeedle := strings.ToLower(q.CWD)
	sourceNeedle := strings.ToLower(q.Source)
	for _, s := range all {
		if len(q.Tools) > 0 && !contains(q.Tools, s.Tool) {
			continue
		}
		if q.Archived != nil && s.Archived != *q.Archived {
			continue
		}
		if q.Since > 0 && s.UpdatedAt < q.Since {
			continue
		}
		if cwdNeedle != "" && !strings.Contains(strings.ToLower(s.CWD), cwdNeedle) {
			continue
		}
		if sourceNeedle != "" && !strings.Contains(strings.ToLower(s.Source), sourceNeedle) {
			continue
		}
		if needle != "" {
			if !strings.Contains(strings.ToLower(s.CWD), needle) &&
				!strings.Contains(strings.ToLower(s.Title), needle) {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

func sortSessions(all []Session, key string) {
	if key == "created" {
		sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
		return
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].UpdatedAt > all[j].UpdatedAt })
}

func pageBounds(offset, limit, total int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	start := min(offset, total)
	end := total
	if limit > 0 {
		end = min(offset+limit, total)
	}
	return start, end
}

// ParseSince parses a since bound: unix ms, YYYY-MM-DD, ISO-8601, relative
// like 7d/12h/30m, or "now". Mirrors the script's --since handling.
func ParseSince(v string) (int64, error) {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return 0, nil
	}
	if raw == "now" {
		return time.Now().UnixMilli(), nil
	}
	if m := sinceRe.FindStringSubmatch(raw); m != nil {
		amount, _ := strconv.ParseFloat(m[1], 64)
		seconds := amount * map[byte]float64{'d': 86400, 'h': 3600, 'm': 60}[m[2][0]]
		return time.Now().Add(-time.Duration(seconds * float64(time.Second))).UnixMilli(), nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil // raw unix ms
	}
	if t := parseISO(raw); t != 0 {
		return t, nil
	}
	return 0, fmt.Errorf("cannot parse %q (want unix ms, YYYY-MM-DD, ISO time, or 7d/12h/30m)", v)
}

var sinceRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([dhm])$`)

// Summary aggregates token usage across sessions, using each tool's own
// cache-rate denominator (codex input already includes the cached subset).
// CachePct is nil when no session has a valid denominator.
type Summary struct {
	Sessions         int      `json:"sessions"`
	TotalTokens      int64    `json:"total_tokens"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	CachePct         *float64 `json:"cache_pct"`
}

// Summarize aggregates token usage across sessions.
func Summarize(sessions []Session) Summary {
	sum := Summary{Sessions: len(sessions)}
	var den int64
	for _, s := range sessions {
		sum.InputTokens += s.InputTokens
		sum.OutputTokens += s.OutputTokens
		sum.CacheReadTokens += s.CacheReadTokens
		sum.CacheWriteTokens += s.CacheWriteTokens
		sum.TotalTokens += s.TokensUsed
		den += cacheDenominator(&s)
	}
	if den > 0 {
		rate := float64(sum.CacheReadTokens) / float64(den)
		sum.CachePct = &rate
	}
	return sum
}
