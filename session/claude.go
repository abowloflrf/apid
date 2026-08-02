package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Claude Code sessions are per-project JSONL transcripts. Scanning reads only
// the first 64KB (meta + title) and the last 8KB (latest timestamp/model), so
// listing stays cheap even for MB-sized transcripts. Token totals are filled
// in later by enrich (full file pass).

const (
	claudeHeadSize = 64 * 1024
	claudeTailSize = 8 * 1024
)

// loadClaude returns Claude Code sessions across all project dirs.
func loadClaude() ([]Session, string) {
	projectsDir := filepath.Join(claudeHome(), "projects")
	matches, err := filepath.Glob(filepath.Join(projectsDir, "**", "*.jsonl"))
	if err != nil {
		return nil, ""
	}
	names := claudeSessionNames()
	var sessions []Session
	for _, path := range matches {
		if s := scanClaudeFast(path, names); s != nil {
			sessions = append(sessions, *s)
		}
	}
	return sessions, "projects/"
}

// claudeSessionNames reads ~/.claude/sessions/*.json, which carries a friendly
// name for active sessions.
func claudeSessionNames() map[string]string {
	names := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(claudeHome(), "sessions", "*.json"))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info struct {
			SessionID string `json:"sessionId"`
			Name      string `json:"name"`
		}
		if json.Unmarshal(data, &info) != nil || info.SessionID == "" || info.Name == "" {
			continue
		}
		names[info.SessionID] = info.Name
	}
	return names
}

// claudeJSONLine is the shared shape of a transcript line (fields of interest).
type claudeJSONLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	Version   string `json:"version"`
	Message   struct {
		Model   string `json:"model"`
		Content any    `json:"content"`
	} `json:"message"`
}

func scanClaudeFast(path string, names map[string]string) *Session {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	head := make([]byte, claudeHeadSize)
	n, _ := f.Read(head)
	head = head[:n]

	fi, _ := f.Stat()
	tailLen := int64(claudeTailSize)
	if fi != nil && fi.Size() < tailLen {
		tailLen = fi.Size()
	}
	tail := make([]byte, tailLen)
	if _, err := f.ReadAt(tail, fi.Size()-tailLen); err != nil && tailLen > 0 {
		tail = nil
	}

	var (
		cwd, version, model, title, createdTS, updatedTS string
		firstUserFound                                   bool
	)
	for _, line := range splitLines(head) {
		var e claudeJSONLine
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Timestamp != "" && createdTS == "" {
			createdTS = e.Timestamp
		}
		if e.CWD != "" {
			cwd = e.CWD
		}
		if e.Version != "" {
			version = e.Version
		}
		switch e.Type {
		case "assistant":
			if e.Message.Model != "" {
				model = e.Message.Model
			}
		case "user":
			if !firstUserFound {
				if text := claudeText(e.Message.Content); text != "" && !strings.HasPrefix(text, "<") {
					title = text
					firstUserFound = true
				}
			}
		}
	}
	for _, line := range splitLines(tail) {
		var e claudeJSONLine
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Timestamp != "" {
			updatedTS = e.Timestamp // last timestamp wins
			if e.Type == "assistant" && e.Message.Model != "" {
				model = e.Message.Model
			}
		}
	}

	if title == "" {
		if name := names[strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))]; name != "" {
			title = name
		}
	}
	if title == "" {
		title = "(无标题)"
	}
	return &Session{
		ID:          strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Title:       title,
		CreatedAt:   parseISO(createdTS),
		UpdatedAt:   parseISO(updatedTS),
		CWD:         cwd,
		Model:       model,
		CliVersion:  version,
		RolloutPath: path,
		Tool:        ToolClaude,
	}
}

// claudeText extracts the first text block from a user message content, which
// is either a plain string or a block list.
func claudeText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if t, ok := m["text"].(string); ok && t != "" {
				return t
			}
		}
	}
	return ""
}

// splitLines splits a byte chunk on '\n', dropping the empty piece produced
// by a trailing newline (a partial last line without newline is kept).
func splitLines(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}
	lines := bytes.Split(chunk, []byte{'\n'})
	if bytes.HasSuffix(chunk, []byte{'\n'}) {
		lines = lines[:len(lines)-1]
	}
	return lines
}
