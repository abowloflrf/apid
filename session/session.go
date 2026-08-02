// Package session lists local AI coding agent sessions (Codex / Claude Code /
// pi / OpenCode) for the dashboard's sessions view. It mirrors the logic of
// scripts/codex_sessions.py in Go so the webui works with no python runtime.
//
// Readers are best-effort and never fail the request: a missing home dir, an
// unreadable file or a schema drift just yields fewer sessions. All stores are
// opened read-only; nothing here ever writes into the user's agent homes.
package session

import (
	"os"
	"regexp"
	"strconv"
	"time"
)

// Tool identifiers, matching the python script's tool field.
const (
	ToolCodex    = "codex"
	ToolClaude   = "claude"
	ToolPi       = "pi"
	ToolOpenCode = "opencode"
)

// Session is one agent session, normalized across all four tools. Field names
// mirror scripts/codex_sessions.py's Session dataclass; timestamps are Unix
// milliseconds (the CLI renders local time strings, the API stays tz-free).
type Session struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	CreatedAt        int64    `json:"created_at_ms"`
	UpdatedAt        int64    `json:"updated_at_ms"`
	CWD              string   `json:"cwd"`
	Model            string   `json:"model"`
	ReasoningEffort  string   `json:"reasoning_effort"`
	TokensUsed       int64    `json:"tokens_used"`
	Archived         bool     `json:"archived"`
	CliVersion       string   `json:"cli_version"`
	RolloutPath      string   `json:"rollout_path"`
	Tool             string   `json:"tool"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	CacheHitRate     *float64 `json:"cache_hit_rate"` // nil = unknown
}

// setTokenFields fills the token columns and derives tokens_used and the cache
// hit rate. denominator semantics differ per tool (see cacheDenominator).
func setTokenFields(s *Session, in, out, cacheRead, cacheWrite, total int64) {
	s.InputTokens = max(0, in)
	s.OutputTokens = max(0, out)
	s.CacheReadTokens = max(0, cacheRead)
	s.CacheWriteTokens = max(0, cacheWrite)
	s.TokensUsed = total
	if s.TokensUsed <= 0 {
		s.TokensUsed = s.InputTokens + s.OutputTokens + s.CacheReadTokens + s.CacheWriteTokens
	}
	den := cacheDenominator(s)
	if den > 0 {
		rate := float64(s.CacheReadTokens) / float64(den)
		s.CacheHitRate = &rate
	}
}

// cacheDenominator is the per-tool basis for the cache hit rate:
//   - codex: input_tokens already includes the cached subset, so it is the
//     denominator by itself (mirrors _set_codex_token_fields).
//   - others: input excludes cache, so cache read+write count on top.
func cacheDenominator(s *Session) int64 {
	if s.Tool == ToolCodex {
		return s.InputTokens
	}
	return s.InputTokens + s.CacheReadTokens + s.CacheWriteTokens
}

// ---- time helpers ----

// parseISO converts an ISO-8601 timestamp (with or without zone) to Unix ms,
// assuming local time when no zone is present. Returns 0 on garbage, matching
// the script's lenient fallbacks. The .999999999 fractional part is optional
// in Go layouts, so one layout covers plain and fractional seconds alike.
func parseISO(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05.999999999Z07:00", s, time.Local)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.Local)
	}
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02", s, time.Local)
	}
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// normalizeEpoch coerces a raw epoch value to ms: second-scale values (about
// 1.7e9 today) get scaled, ms values pass through. Mirrors the script's
// mixed-units tolerance without its broken µs branch.
func normalizeEpoch(v int64) int64 {
	switch {
	case v >= 1_000_000_000_000: // already ms
		return v
	case v >= 1_000_000_000: // seconds
		return v * 1000
	default:
		return 0
	}
}

var versionRe = regexp.MustCompile(`_(\d+)\.sqlite$`)

// stateVersion extracts the version from a state_5.sqlite name; 0 if absent.
func stateVersion(name string) int {
	m := versionRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func homeOr(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
