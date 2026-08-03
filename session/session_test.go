package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseISO(t *testing.T) {
	cases := []struct {
		in   string
		want int64 // unix ms; 0 = expect failure
	}{
		{"", 0},
		{"2026-05-01T10:00:00Z", 1777629600000},
		{"2026-05-01T10:00:00.901Z", 1777629600901},
		{"2026-05-01T10:00:00+08:00", 1777600800000},
		{"2026-05-01", time.Date(2026, 5, 1, 0, 0, 0, 0, time.Local).UnixMilli()}, // date-only, local midnight
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := parseISO(c.in); got != c.want {
			t.Errorf("parseISO(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNormalizeEpoch(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{1777615200000, 1777615200000}, // ms passes through
		{1777615200, 1777615200000},    // seconds scaled
		{42, 0},                        // garbage
	}
	for _, c := range cases {
		if got := normalizeEpoch(c.in); got != c.want {
			t.Errorf("normalizeEpoch(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestStateVersion(t *testing.T) {
	if stateVersion("state_5.sqlite") != 5 {
		t.Error("state_5.sqlite -> 5")
	}
	if stateVersion("state_x.sqlite") != 0 || stateVersion("state.sqlite") != 0 {
		t.Error("non-numeric versions -> 0")
	}
}

func TestCodexRolloutDiscoveryIsRecursive(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(root, "sqlite"))
	path := filepath.Join(root, "sessions", "2026", "08", "03", "rollout-deep.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"session_meta","payload":{"id":"deep","timestamp":"2026-08-03T10:00:00Z","source":"vscode","model_provider":"apid","cwd":"/work/deep","cli_version":"1.2.3"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions, desc := loadCodex()
	if desc != "sessions/ (JSONL)" || len(sessions) != 1 {
		t.Fatalf("loadCodex = %q, %+v", desc, sessions)
	}
	got := sessions[0]
	if got.ID != "deep" || got.Source != "vscode" || got.ModelProvider != "apid" || got.RolloutPath != path {
		t.Errorf("session = %+v", got)
	}
}

func TestParsePiFilenameTime(t *testing.T) {
	for _, value := range []string{"2026-05-23T15-55-35-901Z", "2026-05-23T15-55-35Z"} {
		if got := parsePiFilenameTime(value); got == 0 {
			t.Errorf("parsePiFilenameTime(%q) = 0", value)
		}
	}
}

func TestPiSessionNameAndProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026-05-23T15-55-35-901Z_pi-id.jsonl")
	content := `{"type":"session_info","name":"Friendly name"}` + "\n"
	content += `{"type":"message","message":{"role":"user","content":"Raw first prompt"}}` + "\n"
	content += `{"type":"model_change","provider":"deepseek","modelId":"v4"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := scanPiSession(path)
	if got == nil {
		t.Fatal("scanPiSession returned nil")
	}
	if got.Title != "Friendly name" || got.ModelProvider != "deepseek" || got.Model != "deepseek/v4" {
		t.Errorf("session = %+v", got)
	}
	if got.CreatedAt == 0 {
		t.Error("filename timestamp was not parsed")
	}
}

func TestUsageFieldsFrom(t *testing.T) {
	// openai-style keys (codex)
	f := usageFieldsFrom(map[string]any{
		"input_tokens": float64(100), "output_tokens": float64(20),
		"cached_input_tokens": float64(50), "cache_write_input_tokens": float64(10),
		"total_tokens": float64(130),
	})
	if f.in != 100 || f.out != 20 || f.cacheRead != 50 || f.cacheWrite != 10 || f.total != 130 {
		t.Errorf("openai-style = %+v", f)
	}
	// camelCase + nested cache (pi)
	f = usageFieldsFrom(map[string]any{
		"input": float64(30), "output": float64(7),
		"cache": map[string]any{"read": float64(20), "write": float64(0)},
	})
	if f.in != 30 || f.out != 7 || f.cacheRead != 20 || f.total != 57 {
		t.Errorf("pi-style = %+v", f)
	}
}

func TestCodexUsageEventMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := `{"timestamp":"2026-08-01T07:58:59.774Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1961369,"cached_input_tokens":1915008,"cache_write_input_tokens":0,"output_tokens":40364,"reasoning_output_tokens":26372,"total_tokens":2001733}}}}` + "\n"
	content += `{"timestamp":"2026-08-01T07:59:00Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"The transcript contains the token_count example."}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	fields, ok := NewEnrichCache().codexUsage(path)
	if !ok {
		t.Fatal("codexUsage returned no usage")
	}
	if fields.in != 1961369 || fields.cacheRead != 1915008 || fields.total != 2001733 {
		t.Errorf("usage = %+v", fields)
	}
}

func TestFilterSortSummarize(t *testing.T) {
	mk := func(id string, updated int64) Session {
		return Session{ID: id, Tool: ToolPi, UpdatedAt: updated, Title: "t", CWD: "/work/x"}
	}
	all := []Session{mk("a", 300), mk("b", 100), mk("c", 200)}

	f := filterSessions(all, Query{Q: "work", Since: 150})
	if len(f) != 2 { // a (300), c (200); b dropped by since
		t.Errorf("filter -> %d, want 2", len(f))
	}
	sortSessions(f, "updated")
	if f[0].ID != "a" || f[1].ID != "c" {
		t.Errorf("sort order = %s,%s", f[0].ID, f[1].ID)
	}

	// tool filter + archived
	arch := mk("d", 400)
	arch.Tool = ToolCodex
	arch.Archived = true
	f = filterSessions(append(all, arch), Query{Tools: []string{ToolPi}})
	if len(f) != 3 {
		t.Errorf("tool filter -> %d, want 3", len(f))
	}
	onlyArch := true
	f = filterSessions(append(all, arch), Query{Archived: &onlyArch})
	if len(f) != 1 || f[0].ID != "d" {
		t.Errorf("archived filter -> %+v", f)
	}
	arch.Source = "vscode"
	f = filterSessions(append(all, arch), Query{CWD: "WORK/X", Source: "VSCODE"})
	if len(f) != 1 || f[0].ID != "d" {
		t.Errorf("cwd/source filter -> %+v", f)
	}

	// summary: cache rate uses per-tool denominators
	a := mk("a", 1)
	a.InputTokens = 100
	a.CacheReadTokens = 60 // codex semantics: rate = 60/100
	a.Tool = ToolCodex
	setTokenFields(&a, 100, 0, 60, 0, 0)
	b := mk("b", 2)
	b.InputTokens = 50
	b.CacheReadTokens = 25 // pi semantics: rate = 25/(50+25)
	setTokenFields(&b, 50, 0, 25, 0, 0)
	sum := Summarize([]Session{a, b})
	if sum.TotalTokens != 235 || sum.CacheReadTokens != 85 {
		t.Errorf("summary = %+v", sum)
	}
	if sum.CachePct == nil || *sum.CachePct != 85.0/175.0 {
		t.Errorf("cache pct = %v, want 85/175", sum.CachePct)
	}
}

func TestParseSince(t *testing.T) {
	if _, err := ParseSince("7d"); err != nil {
		t.Errorf("7d: %v", err)
	}
	if _, err := ParseSince("12h"); err != nil {
		t.Errorf("12h: %v", err)
	}
	if _, err := ParseSince("1700000000000"); err != nil {
		t.Errorf("unix ms: %v", err)
	}
	if _, err := ParseSince("2026-05-01"); err != nil {
		t.Errorf("date: %v", err)
	}
	if _, err := ParseSince("garbage"); err == nil {
		t.Error("garbage should fail")
	}
	if _, err := ParseSince(""); err != nil {
		t.Errorf("empty: %v", err)
	}
}
