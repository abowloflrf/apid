package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// pi sessions are per-cwd JSONL transcripts under ~/.pi/agent/sessions. The
// scan walks every line once: session meta, session_info (name), model_change
// and assistant messages all contribute fields.

// messageText flattens a message content (string or block list) to text.
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, block := range v {
			m, ok := block.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// piEntryTime extracts the entry timestamp: integer ms or ISO string.
func piEntryTime(entry map[string]any) int64 {
	switch ts := entry["timestamp"].(type) {
	case float64:
		return normalizeEpoch(int64(ts))
	case int64:
		return normalizeEpoch(ts)
	case string:
		return parseISO(ts)
	}
	return 0
}

// loadPi returns sessions from the pi agent's JSONL transcripts.
func loadPi() ([]Session, string) {
	sessionsDir := filepath.Join(piHome(), "sessions")
	matches, err := filepath.Glob(filepath.Join(sessionsDir, "**", "*.jsonl"))
	if err != nil {
		return nil, ""
	}
	var sessions []Session
	for _, path := range matches {
		if s := scanPiSession(path); s != nil {
			sessions = append(sessions, *s)
		}
	}
	return sessions, "sessions/"
}

func scanPiSession(path string) *Session {
	sessionID := filepath.Base(path)
	if i := strings.LastIndexByte(sessionID, '_'); i >= 0 {
		sessionID = sessionID[i+1:]
	}
	sessionID = strings.TrimSuffix(sessionID, filepath.Ext(sessionID))
	var (
		title, cwd, model, provider, name string
		createdAt, updatedAt              int64
	)

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var entry map[string]any
		if json.Unmarshal(sc.Bytes(), &entry) != nil {
			continue
		}
		entryTime := piEntryTime(entry)
		switch entry["type"] {
		case "session":
			if id, ok := entry["id"].(string); ok && id != "" {
				sessionID = id
			}
			if c, ok := entry["cwd"].(string); ok {
				cwd = c
			}
			if entryTime != 0 {
				createdAt = entryTime
			}
		case "session_info":
			if n, ok := entry["name"].(string); ok {
				name = strings.TrimSpace(n)
			}
		case "model_change":
			if p, ok := entry["provider"].(string); ok {
				provider = p
			}
			if m, ok := entry["modelId"].(string); ok {
				model = m
			}
		case "message":
			msg, _ := entry["message"].(map[string]any)
			if msg == nil {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "user" && title == "" {
				text := messageText(msg["content"])
				if text != "" && !strings.HasPrefix(text, "<") {
					title = text
				}
			} else if role == "assistant" {
				if m, ok := msg["responseModel"].(string); ok && m != "" {
					model = m
				} else if m, ok := msg["model"].(string); ok && m != "" {
					model = m
				}
				if p, ok := msg["provider"].(string); ok {
					provider = p
				}
			}
		}
		if entryTime != 0 {
			updatedAt = entryTime
		}
	}

	if createdAt == 0 {
		// filename timestamp fallback: 2026-05-23T15-55-35-901Z_<uuid>.jsonl
		// -> parse the leading timestamp, or at least its date part.
		stem := filepath.Base(path)
		if i := strings.LastIndexByte(stem, '_'); i > 0 {
			stem = stem[:i]
		}
		createdAt = parseISO(stem)
		if createdAt == 0 && len(stem) >= 10 {
			createdAt = parseISO(stem[:10])
		}
	}
	if updatedAt == 0 {
		if fi, err := os.Stat(path); err == nil {
			updatedAt = fi.ModTime().UnixMilli()
		} else {
			updatedAt = createdAt
		}
	}

	if model != "" && provider != "" && !strings.Contains(model, "/") {
		model = provider + "/" + model
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = name
	}
	if title == "" {
		title = "(无标题)"
	}
	return &Session{
		ID:          sessionID,
		Title:       title,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
		CWD:         cwd,
		Model:       model,
		RolloutPath: path,
		Tool:        ToolPi,
	}
}
