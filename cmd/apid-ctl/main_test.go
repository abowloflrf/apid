package main

import (
	"bytes"
	"encoding/json"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/abowloflrf/apid/session"
	"github.com/mattn/go-runewidth"
)

func TestRunJSONFromRecursiveCodexRollout(t *testing.T) {
	root := t.TempDir()
	setSessionHomes(t, root)
	rollout := filepath.Join(root, "codex", "sessions", "2026", "08", "03", "rollout-test.jsonl")
	writeTestFile(t, rollout,
		`{"type":"session_meta","payload":{"id":"session-id","timestamp":"2026-08-03T10:00:00Z","source":"vscode","model_provider":"apid","cwd":"/work/apid","cli_version":"1.0.0"}}`+"\n"+
			`{"type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":10,"output_tokens":2,"cached_input_tokens":5,"total_tokens":12}}}}`+"\n")
	writeTestFile(t, filepath.Join(root, "codex", "sessions", "zero", "rollout-zero.jsonl"),
		`{"type":"session_meta","payload":{"id":"zero","timestamp":"2026-08-03T11:00:00Z","source":"cli","cwd":"/work/zero"}}`+"\n")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--tool", "codex", "--source", "VSCODE", "--json"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run: %v (%s)", err, stderr.String())
	}
	var got []outputSession
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if len(got) != 1 {
		t.Fatalf("sessions = %+v", got)
	}
	s := got[0]
	if s.ID != "session-id" || s.Title != "apid" || s.Source != "vscode" || s.ModelProvider != "apid" ||
		s.TokensUsed != 12 || s.CacheHitRate == nil || *s.CacheHitRate != 0.5 || s.CreatedAt == "" {
		t.Errorf("session = %+v", s)
	}
}

func TestRunPlainReport(t *testing.T) {
	root := t.TempDir()
	setSessionHomes(t, root)
	rollout := filepath.Join(root, "codex", "sessions", "sub", "rollout-test.jsonl")
	writeTestFile(t, rollout,
		`{"type":"session_meta","payload":{"id":"plain-id","timestamp":"2026-08-03T10:00:00Z","cwd":"/work/plain"}}`+"\n"+
			`{"type":"token_count","payload":{"info":{"total_token_usage":{"input_tokens":1000,"output_tokens":20,"total_tokens":1020}}}}`+"\n")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"--tool", "codex", "--plain"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"数据源: Codex: sessions/ (JSONL)", "plain-id", "1K", "共 1 条会话", "Token 总计"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Errorf("plain output contains ANSI styling:\n%q", stdout.String())
	}
}

func TestPlainReportCleansAndTruncatesMultilineTitle(t *testing.T) {
	t.Setenv("COLUMNS", "64")
	title := strings.Repeat("第一行很长的标题\n第二行仍然是标题 ", 8)
	sessions := []session.Session{{
		ID:        "multiline-session",
		Title:     title,
		UpdatedAt: 1785741600000,
		Tool:      session.ToolCodex,
	}}

	var output bytes.Buffer
	writeReport(&output, nil, sessions)
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("multiline title broke report into %d lines:\n%s", len(lines), output.String())
	}
	header, row := lines[2], lines[3]
	if runewidth.StringWidth(header) > 64 || runewidth.StringWidth(row) > 64 {
		t.Fatalf("plain table exceeds terminal width:\n%s\n%s", header, row)
	}
	if !strings.Contains(row, "…") {
		t.Fatalf("long title was not truncated:\n%s", row)
	}
	if strings.Contains(row, "\t") || strings.Contains(row, "\r") {
		t.Fatalf("row contains control whitespace: %q", row)
	}
}

func TestPlainTableCanUsePythonPalette(t *testing.T) {
	cache := 0.95
	s := session.Session{
		ID:           "colored-id",
		Title:        "colored title",
		UpdatedAt:    1785741600000,
		Tool:         session.ToolCodex,
		TokensUsed:   1_000_000,
		CacheHitRate: &cache,
	}

	var output bytes.Buffer
	writePlainTable(&output, []session.Session{s}, 128, true)
	checks := []string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Cyan).Render(plainCell("时间", 16)),
		sessionCellStyle(s, fieldTool).Render(plainCell("CX", 2)),
		sessionCellStyle(s, fieldTokens).Render(plainCell("1.0M", 7)),
		sessionCellStyle(s, fieldCache).Render(plainCell("95.0%", 7)),
	}
	for _, want := range checks {
		if !strings.Contains(output.String(), want) {
			t.Errorf("colored plain table missing %q:\n%q", want, output.String())
		}
	}
}

func TestPlainColorPolicy(t *testing.T) {
	var output bytes.Buffer
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	if plainColorEnabled(&output) {
		t.Error("non-terminal output should not use color")
	}
	t.Setenv("FORCE_COLOR", "1")
	if !plainColorEnabled(&output) {
		t.Error("FORCE_COLOR should enable color")
	}
	t.Setenv("NO_COLOR", "1")
	if plainColorEnabled(&output) {
		t.Error("NO_COLOR should take precedence")
	}
}

func TestParseOptionsAndTUIResize(t *testing.T) {
	opts, err := parseOptions([]string{"--agent", "pi", "--limit", "3", "--sort", "created"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.tool != "pi" || opts.limit != 3 || opts.sort != "created" {
		t.Errorf("options = %+v", opts)
	}
	if _, err := parseOptions([]string{"--tool", "unknown"}, &bytes.Buffer{}); err == nil {
		t.Error("invalid tool should fail")
	}

	m := newTUIModel(nil, []session.Session{{ID: "id", Title: "title", Tool: session.ToolCodex}})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	got := updated.(tuiModel)
	if len(got.table.Columns()) != 4 {
		t.Errorf("narrow columns = %d, want 4", len(got.table.Columns()))
	}
	view := got.View()
	if !view.AltScreen || !strings.Contains(view.Content, "q/Esc") {
		t.Errorf("view = %+v", view)
	}
}

func TestSessionTableStylesMatchPythonPalette(t *testing.T) {
	styles := sessionTableStyles()
	if !styles.Header.GetBold() || styles.Header.GetForeground() != lipgloss.Cyan {
		t.Errorf("header style = bold:%v color:%v", styles.Header.GetBold(), styles.Header.GetForeground())
	}
	if !styles.Selected.GetReverse() {
		t.Error("selected row should use reverse video")
	}

	highCache, mediumCache, lowCache := 0.95, 0.75, 0.50
	tests := []struct {
		name      string
		field     tableField
		session   session.Session
		color     color.Color
		wantBold  bool
		wantFaint bool
	}{
		{name: "time", field: fieldTime, wantFaint: true},
		{name: "codex", field: fieldTool, session: session.Session{Tool: session.ToolCodex}, color: lipgloss.Magenta},
		{name: "claude", field: fieldTool, session: session.Session{Tool: session.ToolClaude}, color: lipgloss.Yellow},
		{name: "pi", field: fieldTool, session: session.Session{Tool: session.ToolPi}, color: lipgloss.Green},
		{name: "opencode", field: fieldTool, session: session.Session{Tool: session.ToolOpenCode}, color: lipgloss.Blue},
		{name: "id", field: fieldID, color: lipgloss.Cyan},
		{name: "million tokens", field: fieldTokens, session: session.Session{TokensUsed: 1_000_000}, color: lipgloss.BrightCyan, wantBold: true},
		{name: "medium tokens", field: fieldTokens, session: session.Session{TokensUsed: 100_000}, color: lipgloss.Cyan},
		{name: "small tokens", field: fieldTokens, session: session.Session{TokensUsed: 99_999}, color: lipgloss.Cyan, wantFaint: true},
		{name: "high cache", field: fieldCache, session: session.Session{CacheHitRate: &highCache}, color: lipgloss.Green},
		{name: "medium cache", field: fieldCache, session: session.Session{CacheHitRate: &mediumCache}, color: lipgloss.Yellow},
		{name: "low cache", field: fieldCache, session: session.Session{CacheHitRate: &lowCache}, color: lipgloss.Red},
		{name: "unknown cache", field: fieldCache, wantFaint: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionCellStyle(tt.session, tt.field)
			if (tt.color != nil && got.GetForeground() != tt.color) || got.GetBold() != tt.wantBold || got.GetFaint() != tt.wantFaint {
				t.Errorf("style = color:%v bold:%v faint:%v", got.GetForeground(), got.GetBold(), got.GetFaint())
			}
		})
	}
}

func setSessionHomes(t *testing.T, root string) {
	t.Helper()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_HOME", filepath.Join(root, "claude"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "pi"))
	t.Setenv("OPENCODE_DATA_HOME", filepath.Join(root, "opencode"))
	t.Setenv("OPENCODE_DB", "")
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
