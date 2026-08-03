package session

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Codex sessions live in state_*.sqlite (threads table) when present, with a
// rollout-*.jsonl fallback for older installs without the state db.

// openReadOnly opens a SQLite file read-only; nil on any failure.
func openReadOnly(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro")
}

func orString(v sql.NullString, fallback string) string {
	if v.Valid && v.String != "" {
		return v.String
	}
	return fallback
}

// loadCodex returns sessions from the newest state db, or from rollout JSONL
// files when no state db exists, plus a short description of the backing store.
func loadCodex() ([]Session, string) {
	if db := findStateDB(); db != "" {
		return loadFromStateDB(db), filepath.Base(db)
	}
	sessionsDir := filepath.Join(codexHome(), "sessions")
	if fi, err := os.Stat(sessionsDir); err == nil && fi.IsDir() {
		return loadFromRolloutFiles(), "sessions/ (JSONL)"
	}
	return nil, ""
}

func loadFromStateDB(dbPath string) []Session {
	conn, err := openReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer conn.Close()

	rows, err := conn.Query(`SELECT id, title, created_at, updated_at,
		source, model_provider, cwd, model, reasoning_effort, tokens_used, archived,
		cli_version, rollout_path FROM threads`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		var title, source, provider, cwd, model, reasoning, cli, rollout sql.NullString
		var created, updated, tokens sql.NullInt64
		var archived int64
		if err := rows.Scan(&s.ID, &title, &created, &updated, &source, &provider,
			&cwd, &model, &reasoning, &tokens, &archived, &cli, &rollout); err != nil {
			continue
		}
		if isCodexGuardianSource(source.String) {
			continue
		}
		s.Title = orString(title, "(无标题)")
		s.CreatedAt = normalizeEpoch(created.Int64)
		s.UpdatedAt = normalizeEpoch(updated.Int64)
		s.Source = source.String
		s.ModelProvider = provider.String
		s.CWD = cwd.String
		s.Model = model.String
		s.ReasoningEffort = reasoning.String
		s.TokensUsed = tokens.Int64
		s.Archived = archived != 0
		s.CliVersion = cli.String
		s.RolloutPath = rollout.String
		s.Tool = ToolCodex
		sessions = append(sessions, s)
	}
	return sessions
}

func isCodexGuardianSource(raw string) bool {
	var source struct {
		Subagent struct {
			Other string `json:"other"`
		} `json:"subagent"`
	}
	return json.Unmarshal([]byte(raw), &source) == nil && source.Subagent.Other == "guardian"
}

// rolloutMeta is the first JSONL line of a rollout file (session_meta event).
type rolloutMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID         string `json:"id"`
		Timestamp  string `json:"timestamp"`
		Source     string `json:"source"`
		Provider   string `json:"model_provider"`
		CWD        string `json:"cwd"`
		CliVersion string `json:"cli_version"`
	} `json:"payload"`
}

func loadFromRolloutFiles() []Session {
	sessionsDir := filepath.Join(codexHome(), "sessions")
	matches := walkFiles(sessionsDir, func(name string) bool {
		return filepath.Ext(name) == ".jsonl" && len(name) > len("rollout-") && name[:len("rollout-")] == "rollout-"
	})
	var sessions []Session
	for _, path := range matches {
		meta, ok := readRolloutMeta(path)
		if !ok {
			continue
		}
		created := parseISO(meta.Payload.Timestamp)
		title := filepath.Base(meta.Payload.CWD)
		if title == "." || title == "/" || title == "" {
			title = "(无标题)"
		}
		sessions = append(sessions, Session{
			ID:            meta.Payload.ID,
			Title:         title,
			CreatedAt:     created,
			UpdatedAt:     created,
			Source:        meta.Payload.Source,
			ModelProvider: meta.Payload.Provider,
			CWD:           meta.Payload.CWD,
			CliVersion:    meta.Payload.CliVersion,
			RolloutPath:   path,
			Tool:          ToolCodex,
		})
	}
	return sessions
}

func readRolloutMeta(path string) (rolloutMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return rolloutMeta{}, false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return rolloutMeta{}, false
	}
	var meta rolloutMeta
	if json.Unmarshal([]byte(line), &meta) != nil || meta.Type != "session_meta" {
		return rolloutMeta{}, false
	}
	return meta, true
}
