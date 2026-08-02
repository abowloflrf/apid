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
		if s := loadFromStateDB(db); len(s) > 0 {
			return s, filepath.Base(db)
		}
	}
	return loadFromRolloutFiles(), "sessions/ (JSONL)"
}

func loadFromStateDB(dbPath string) []Session {
	conn, err := openReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer conn.Close()

	rows, err := conn.Query(`SELECT id, title, created_at, updated_at,
		cwd, model, reasoning_effort, tokens_used, archived,
		cli_version, rollout_path FROM threads`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		var title, cwd, model, reasoning, cli, rollout sql.NullString
		var created, updated, tokens sql.NullInt64
		var archived int64
		if err := rows.Scan(&s.ID, &title, &created, &updated,
			&cwd, &model, &reasoning, &tokens, &archived, &cli, &rollout); err != nil {
			continue
		}
		s.Title = orString(title, "(无标题)")
		s.CreatedAt = normalizeEpoch(created.Int64)
		s.UpdatedAt = normalizeEpoch(updated.Int64)
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

// rolloutMeta is the first JSONL line of a rollout file (session_meta event).
type rolloutMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID         string `json:"id"`
		Timestamp  string `json:"timestamp"`
		CWD        string `json:"cwd"`
		CliVersion string `json:"cli_version"`
	} `json:"payload"`
}

func loadFromRolloutFiles() []Session {
	sessionsDir := filepath.Join(codexHome(), "sessions")
	matches, err := filepath.Glob(filepath.Join(sessionsDir, "**", "rollout-*.jsonl"))
	if err != nil {
		return nil
	}
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
			ID:          meta.Payload.ID,
			Title:       title,
			CreatedAt:   created,
			UpdatedAt:   created,
			CWD:         meta.Payload.CWD,
			CliVersion:  meta.Payload.CliVersion,
			RolloutPath: path,
			Tool:        ToolCodex,
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
