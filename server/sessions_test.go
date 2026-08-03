package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/store"

	_ "modernc.org/sqlite"
)

// sessionsTestEnv points every tool home at a fresh temp dir.
func sessionsTestEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CLAUDE_HOME", filepath.Join(home, "claude"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi"))
	t.Setenv("OPENCODE_DATA_HOME", filepath.Join(home, "opencode"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))
	return home
}

func sessionsServer(t *testing.T) *Server {
	t.Helper()
	return New(config.Config{}, &store.Store{}, nil)
}

func getSessions(t *testing.T, s *Server, query string) sessionsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stats/sessions?"+query, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stats/sessions?%s = %d: %s", query, rec.Code, rec.Body.String())
	}
	var out sessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return out
}

func mustExec(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSessionsCodexStateDB: threads table read through the endpoint, with the
// token columns coming straight from the db.
func TestSessionsCodexStateDB(t *testing.T) {
	env := sessionsTestEnv(t)
	codex := filepath.Join(env, "codex")
	mkdirAll(t, codex)
	db, err := sql.Open("sqlite", filepath.Join(codex, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE threads (
		id TEXT, title TEXT, created_at INTEGER, updated_at INTEGER, source TEXT,
		model_provider TEXT, cwd TEXT, model TEXT, reasoning_effort TEXT,
		tokens_used INTEGER, archived INTEGER, cli_version TEXT, rollout_path TEXT)`)
	mustExec(t, db, `INSERT INTO threads VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"codex-1", "Codex session", 1700000000000, 1700003600000, "cli", "openai",
		"/tmp/proj", "gpt-5", "medium", 1000, 0, "0.1.0", "/tmp/rollout-1.jsonl")
	mustExec(t, db, `INSERT INTO threads VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"codex-2", "Archived", 1700000000000, 1700003700000, "vscode", "openai",
		"/tmp/proj2", "gpt-4o", "", 500, 1, "0.1.0", "/tmp/rollout-2.jsonl")
	mustExec(t, db, `INSERT INTO threads VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"review-1", "Internal approval review", 1700000000000, 1700003800000,
		`{"subagent":{"other":"guardian"}}`, "openai", "/tmp/proj", "glm-5.2", "medium",
		200, 0, "0.1.0", "/tmp/rollout-review.jsonl")

	out := getSessions(t, sessionsServer(t), "tool=codex")
	if out.Total != 2 || len(out.Sessions) != 2 {
		t.Fatalf("total = %d (%d returned), want 2", out.Total, len(out.Sessions))
	}
	// default sort: updated desc -> codex-2 first
	if s := out.Sessions[0]; s.ID != "codex-2" || !s.Archived || s.TokensUsed != 500 {
		t.Errorf("first session = %+v", s)
	}
	if s := out.Sessions[1]; s.ID != "codex-1" || s.Archived || s.Title != "Codex session" ||
		s.Model != "gpt-5" || s.Source != "cli" || s.ModelProvider != "openai" ||
		s.TokensUsed != 1000 || s.CliVersion != "0.1.0" {
		t.Errorf("second session = %+v", s)
	}
	if s := out.Sessions[1]; s.CacheHitRate != nil {
		t.Errorf("cache rate without rollout usage should be nil, got %v", *s.CacheHitRate)
	}
	if len(out.Sources) != 1 || out.Sources[0].Desc != "state_5.sqlite" {
		t.Errorf("sources = %+v", out.Sources)
	}

	// archived filter
	archived := getSessions(t, sessionsServer(t), "tool=codex&archived=1")
	if archived.Total != 1 || archived.Sessions[0].ID != "codex-2" {
		t.Errorf("archived=1 -> %+v", archived.Sessions)
	}
	// paging
	paged := getSessions(t, sessionsServer(t), "tool=codex&limit=1&offset=1")
	if paged.Total != 2 || len(paged.Sessions) != 1 || paged.Sessions[0].ID != "codex-1" {
		t.Errorf("paged -> total %d, %d rows", paged.Total, len(paged.Sessions))
	}
	sourced := getSessions(t, sessionsServer(t), "tool=codex&source=vscode&cwd=proj2")
	if sourced.Total != 1 || sourced.Sessions[0].ID != "codex-2" {
		t.Errorf("source/cwd filter -> %+v", sourced.Sessions)
	}
}

// TestSessionsCodexRolloutFallback: no state db -> session_meta JSONL scan,
// with_token_count enrichment from the last token_count event.
func TestSessionsCodexRolloutFallback(t *testing.T) {
	env := sessionsTestEnv(t)
	codex := filepath.Join(env, "codex")
	rollout := filepath.Join(codex, "sessions", "sub", "rollout-abc.jsonl")
	writeFile(t, rollout, `{"type":"session_meta","payload":{"id":"r-1","timestamp":"2026-05-01T10:00:00Z","cwd":"/work/foo","source":"cli","model_provider":"openai","cli_version":"0.2"}}
{"type":"agent_thought","payload":{"text":"hello"}}
{"type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":100,"output_tokens":20,"cached_input_tokens":50,"cache_write_input_tokens":10,"total_tokens":130}}}}
`)

	out := getSessions(t, sessionsServer(t), "tool=codex")
	if out.Total != 1 {
		t.Fatalf("total = %d, want 1", out.Total)
	}
	s := out.Sessions[0]
	if s.ID != "r-1" || s.Title != "foo" || s.CWD != "/work/foo" || s.CliVersion != "0.2" {
		t.Errorf("session = %+v", s)
	}
	if s.TokensUsed != 0 {
		t.Errorf("tokens without with_tokens should stay 0, got %d", s.TokensUsed)
	}
	if out.Sources[0].Desc != "sessions/ (JSONL)" {
		t.Errorf("source = %+v", out.Sources[0])
	}

	enriched := getSessions(t, sessionsServer(t), "tool=codex&with_tokens=1")
	s = enriched.Sessions[0]
	if s.TokensUsed != 130 || s.InputTokens != 100 || s.OutputTokens != 20 ||
		s.CacheReadTokens != 50 || s.CacheWriteTokens != 10 {
		t.Errorf("enriched tokens = %+v", s)
	}
	// codex denominator = input (includes the cached subset)
	if s.CacheHitRate == nil || *s.CacheHitRate != 0.5 {
		t.Errorf("cache rate = %v, want 0.5", s.CacheHitRate)
	}
}

func TestSessionsWithTokensFiltersZeroTokenSessions(t *testing.T) {
	env := sessionsTestEnv(t)
	codex := filepath.Join(env, "codex")
	valid := filepath.Join(codex, "sessions", "sub", "rollout-valid.jsonl")
	zero := filepath.Join(codex, "sessions", "sub", "rollout-zero.jsonl")
	writeFile(t, valid, `{"type":"session_meta","payload":{"id":"valid","timestamp":"2026-05-01T10:00:00Z","cwd":"/work/valid"}}
{"type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":100,"output_tokens":20,"cached_input_tokens":50,"total_tokens":120}}}}
`)
	writeFile(t, zero, `{"type":"session_meta","payload":{"id":"zero","timestamp":"2026-05-01T11:00:00Z","cwd":"/work/zero"}}
`)

	all := getSessions(t, sessionsServer(t), "tool=codex&with_tokens=0")
	if all.Total != 2 {
		t.Fatalf("without enrichment total = %d, want 2", all.Total)
	}

	enriched := getSessions(t, sessionsServer(t), "tool=codex&with_tokens=1")
	if enriched.Total != 1 || len(enriched.Sessions) != 1 || enriched.Sessions[0].ID != "valid" {
		t.Fatalf("with enrichment sessions = %+v, total = %d; want only valid", enriched.Sessions, enriched.Total)
	}
	if enriched.Summary.TotalTokens != 120 {
		t.Errorf("summary total_tokens = %d, want 120", enriched.Summary.TotalTokens)
	}
}

// TestSessionsClaudePi: JSONL transcripts for claude (fast scan) and pi.
func TestSessionsClaudePi(t *testing.T) {
	env := sessionsTestEnv(t)

	// claude: head carries meta+title, tail carries the latest timestamp.
	claudeProj := filepath.Join(env, "claude", "projects", "-work-foo")
	writeFile(t, filepath.Join(claudeProj, "claude-1.jsonl"),
		`{"type":"summary","timestamp":"2026-05-01T09:00:00+08:00","cwd":"/work/foo","version":"1.0.30"}
{"type":"user","timestamp":"2026-05-01T09:01:00+08:00","message":{"content":"Fix the build"}}
{"type":"assistant","timestamp":"2026-05-01T09:02:00+08:00","message":{"model":"claude-opus-4","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":1}}}
{"type":"assistant","timestamp":"2026-05-01T09:03:00+08:00","message":{"model":"claude-opus-4","usage":{"input_tokens":2,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
`)
	// claude session name from sessions/*.json (used when no user message gives a title)
	writeFile(t, filepath.Join(env, "claude", "sessions", "claude-1.json"),
		`{"sessionId":"claude-1","name":"Build fix session"}`)
	// a second claude session with no user text -> falls back to the session name
	writeFile(t, filepath.Join(claudeProj, "claude-2.jsonl"),
		`{"type":"summary","timestamp":"2026-05-01T10:00:00+08:00","cwd":"/work/foo","version":"1.0.30"}`)
	writeFile(t, filepath.Join(env, "claude", "sessions", "claude-2.json"),
		`{"sessionId":"claude-2","name":"Named session"}`)

	// pi transcript
	piDir := filepath.Join(env, "pi", "sessions", "--work-foo--")
	writeFile(t, filepath.Join(piDir, "2026-05-02T10-00-00-000Z_pi-1.jsonl"),
		`{"type":"session","id":"pi-1","cwd":"/work/foo","timestamp":"2026-05-02T10:00:00.000Z"}
{"type":"model_change","provider":"deepseek","modelId":"v4","timestamp":"2026-05-02T10:00:01.000Z"}
{"type":"message","timestamp":"2026-05-02T10:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"Refactor main"}]}}
{"type":"message","timestamp":"2026-05-02T10:00:03.000Z","message":{"role":"assistant","model":"v4","provider":"deepseek","usage":{"input":30,"output":7,"cacheRead":20,"cacheWrite":0,"totalTokens":57}}}
{"type":"branch_summary","timestamp":"2026-05-02T10:00:04.000Z","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}
`)

	s := sessionsServer(t)
	out := getSessions(t, s, "tool=claude&tool=pi")
	if out.Total != 3 {
		t.Fatalf("total = %d, want 3: %+v", out.Total, out.Sessions)
	}
	var claude, pi bool
	for _, x := range out.Sessions {
		switch x.Tool {
		case "claude":
			claude = true
			if x.ID == "claude-1" {
				// first user message beats the session name
				if x.Title != "Fix the build" {
					t.Errorf("claude-1 title = %q, want the user message", x.Title)
				}
			}
			if x.ID == "claude-2" && x.Title != "Named session" {
				t.Errorf("claude-2 title = %q, want session name", x.Title)
			}
			if x.ID == "claude-1" && x.Model != "claude-opus-4" {
				t.Errorf("claude-1 model = %q", x.Model)
			}
			if x.CliVersion != "1.0.30" || x.CWD != "/work/foo" {
				t.Errorf("claude session = %+v", x)
			}
		case "pi":
			pi = true
			if x.ID != "pi-1" || x.Model != "deepseek/v4" || x.CWD != "/work/foo" {
				t.Errorf("pi session = %+v", x)
			}
		}
	}
	if !claude || !pi {
		t.Fatalf("expected both tools, got %+v", out.Sessions)
	}

	enriched := getSessions(t, s, "tool=claude&tool=pi&with_tokens=1")
	for _, x := range enriched.Sessions {
		switch x.Tool {
		case "claude":
			if x.ID != "claude-1" {
				continue // claude-2 has no usage data, nothing to enrich
			}
			// 12 in / 6 out / 3 cache-read / 1 cache-write; denominator = in+cr+cw
			if x.TokensUsed != 22 || x.InputTokens != 12 || x.OutputTokens != 6 ||
				x.CacheReadTokens != 3 || x.CacheWriteTokens != 1 {
				t.Errorf("claude tokens = %+v", x)
			}
			if x.CacheHitRate == nil || *x.CacheHitRate != 3.0/16.0 {
				t.Errorf("claude cache rate = %v, want 0.1875", *x.CacheHitRate)
			}
		case "pi":
			// 31 in / 8 out / 20 cache-read / 0 write; denominator = in+cr+cw
			if x.TokensUsed != 59 || x.InputTokens != 31 || x.OutputTokens != 8 ||
				x.CacheReadTokens != 20 {
				t.Errorf("pi tokens = %+v", x)
			}
			if x.CacheHitRate == nil || *x.CacheHitRate != 20.0/51.0 {
				t.Errorf("pi cache rate = %v, want ~0.392", *x.CacheHitRate)
			}
		}
	}
}

// TestSessionsOpenCodeDB: opencode sqlite store with a JSON-encoded model.
func TestSessionsOpenCodeDB(t *testing.T) {
	env := sessionsTestEnv(t)
	dbPath := filepath.Join(env, "opencode", "opencode.db")
	mkdirAll(t, filepath.Dir(dbPath))
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE session (
		id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER,
		time_archived INTEGER, version TEXT, agent TEXT, model TEXT,
		tokens_input INTEGER, tokens_output INTEGER, tokens_reasoning INTEGER,
		tokens_cache_read INTEGER, tokens_cache_write INTEGER, cost REAL)`)
	mustExec(t, db, `INSERT INTO session VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"oc-1", "/work/oc", "OC session", 1700000000000, 1700003600000, 0, "0.9.0", "build",
		`{"id":"gpt-4o","providerID":"openai"}`, 100, 20, 5, 30, 2, 0.5)

	out := getSessions(t, sessionsServer(t), "tool=opencode")
	if out.Total != 1 {
		t.Fatalf("total = %d, want 1", out.Total)
	}
	s := out.Sessions[0]
	// tokens come from the db even without with_tokens; reasoning folded into output
	if s.Model != "openai/gpt-4o" {
		t.Errorf("model = %q", s.Model)
	}
	if s.TokensUsed != 157 || s.OutputTokens != 25 || s.CacheReadTokens != 30 || s.CacheWriteTokens != 2 {
		t.Errorf("tokens = %+v", s)
	}
}

// TestSessionsFilters: q substring, since, sort=created, invalid params.
func TestSessionsFilters(t *testing.T) {
	env := sessionsTestEnv(t)
	codex := filepath.Join(env, "codex")
	mkdirAll(t, codex)
	db, err := sql.Open("sqlite", filepath.Join(codex, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE threads (
		id TEXT, title TEXT, created_at INTEGER, updated_at INTEGER, source TEXT,
		model_provider TEXT, cwd TEXT, model TEXT, reasoning_effort TEXT,
		tokens_used INTEGER, archived INTEGER, cli_version TEXT, rollout_path TEXT)`)
	mk := func(id, title, cwd string, created, updated int64) {
		mustExec(t, db, `INSERT INTO threads VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, title, created, updated, "cli", "openai", cwd, "gpt-5", "", 10, 0, "0.1", "")
	}
	now := time.Now().UnixMilli()
	hour := int64(3600e3)
	mk("a", "Alpha work", "/work/alpha", now-3*24*hour, now-2*hour)
	mk("b", "Beta things", "/work/beta", now-3*24*hour, now-1*hour)
	mk("c", "Gamma", "/home/user/alpha", now-3*24*hour, now-30*60e3)

	s := sessionsServer(t)
	// q matches cwd and title, case-insensitive
	q := getSessions(t, s, "tool=codex&q=alpha")
	if q.Total != 2 {
		t.Errorf("q=alpha -> %d, want 2", q.Total)
	}
	// since (unix ms) filters on updated with >= semantics: only b and c qualify
	since := getSessions(t, s, "tool=codex&since="+strconv.FormatInt(now-2*hour+1, 10))
	if since.Total != 2 {
		t.Errorf("since -> %d, want 2", since.Total)
	}
	// sort=created flips the order (all created equal -> stable, keep a,b,c)
	created := getSessions(t, s, "tool=codex&sort=created")
	if created.Sessions[0].ID != "a" {
		t.Errorf("sort=created first = %s, want a", created.Sessions[0].ID)
	}
	// relative since works too
	rel := getSessions(t, s, "tool=codex&since=7d")
	if rel.Total != 3 {
		t.Errorf("since=7d -> %d, want 3", rel.Total)
	}

	for _, bad := range []string{"tool=nope", "archived=x", "limit=-1", "sort=foo", "since=garbage"} {
		req := httptest.NewRequest(http.MethodGet, "/stats/sessions?"+bad, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", bad, rec.Code)
		}
	}
}

// TestSessionsEmptyHome: nothing installed -> empty result, no sources, 200.
func TestSessionsEmptyHome(t *testing.T) {
	sessionsTestEnv(t)
	out := getSessions(t, sessionsServer(t), "")
	if out.Total != 0 || len(out.Sessions) != 0 || len(out.Sources) != 0 {
		t.Errorf("expected empty result, got total=%d sources=%+v", out.Total, out.Sources)
	}
	if out.Generated == 0 || time.Since(time.UnixMilli(out.Generated)) > time.Minute {
		t.Errorf("generated_ms not fresh: %d", out.Generated)
	}
}
