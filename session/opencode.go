package session

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// loadOpenCode returns sessions from the opencode database, falling back to the
// pre-db storage/session/info JSON format, plus a store description.
func loadOpenCode() ([]Session, string) {
	if db := findOpencodeDB(); db != "" {
		return loadOpenCodeDB(db), db
	}
	storage := filepath.Join(opencodeDataHome(), "storage")
	if fi, err := os.Stat(storage); err == nil && fi.IsDir() {
		return loadOpenCodeLegacy(), storage + string(filepath.Separator)
	}
	return nil, ""
}

// opencodeModel decodes the model column, which is either a plain model id or
// a JSON string like {"id":..,"providerID":..}.
func opencodeModel(value any) (model, provider string) {
	raw := ""
	switch v := value.(type) {
	case string:
		raw = strings.TrimSpace(v)
	case []byte:
		raw = strings.TrimSpace(string(v))
	default:
		return "", ""
	}
	if !strings.HasPrefix(raw, "{") {
		return raw, ""
	}
	var m struct {
		ID         string `json:"id"`
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
		Provider   string `json:"provider"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return raw, ""
	}
	model = m.ID
	if model == "" {
		model = m.ModelID
	}
	provider = m.ProviderID
	if provider == "" {
		provider = m.Provider
	}
	if provider != "" && model != "" {
		return provider + "/" + model, provider
	}
	if model != "" {
		return model, provider
	}
	return provider, provider
}

func loadOpenCodeDB(dbPath string) []Session {
	conn, err := openReadOnly(dbPath)
	if err != nil {
		return nil
	}
	defer conn.Close()

	cols, err := columnNames(conn, "session")
	if err != nil {
		return nil
	}
	required := []string{"id", "directory", "title", "time_created", "time_updated"}
	for _, c := range required {
		if !contains(cols, c) {
			return nil
		}
	}
	selected := []string{
		"id", "directory", "title", "time_created", "time_updated",
		"time_archived", "version", "agent", "model",
	}
	tokenCols := []string{
		"cost", "tokens_input", "tokens_output", "tokens_reasoning",
		"tokens_cache_read", "tokens_cache_write",
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c] = true
	}
	for _, c := range tokenCols {
		if have[c] {
			selected = append(selected, c)
		}
	}

	rows, err := conn.Query("SELECT " + strings.Join(selected, ", ") + " FROM session")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		vals := make([]any, len(selected))
		ptrs := make([]any, len(selected))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		s, ok := openCodeSession(selected, vals, dbPath)
		if ok {
			sessions = append(sessions, s)
		}
	}
	return sessions
}

func columnNames(conn *sql.DB, table string) ([]string, error) {
	rows, err := conn.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk) == nil {
			names = append(names, name)
		}
	}
	return names, rows.Err()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func openCodeSession(selected []string, vals []any, dbPath string) (Session, bool) {
	get := func(name string) any {
		for i, c := range selected {
			if c == name {
				return vals[i]
			}
		}
		return nil
	}
	str := func(v any) string {
		switch s := v.(type) {
		case string:
			return s
		case []byte:
			return string(s)
		}
		return ""
	}
	ts := func(v any) int64 {
		switch n := v.(type) {
		case int64:
			return normalizeEpoch(n)
		case float64:
			return normalizeEpoch(int64(n))
		}
		return 0
	}
	model, provider := opencodeModel(get("model"))
	var s Session
	s.ID = str(get("id"))
	s.Title = str(get("title"))
	if s.Title == "" {
		s.Title = "(无标题)"
	}
	s.CreatedAt = ts(get("time_created"))
	s.UpdatedAt = ts(get("time_updated"))
	s.CWD = str(get("directory"))
	s.Model = model
	s.ModelProvider = provider
	s.Archived = ts(get("time_archived")) != 0
	s.CliVersion = str(get("version"))
	s.RolloutPath = dbPath
	s.Tool = ToolOpenCode

	in, _ := intValue(get("tokens_input"))
	out, _ := intValue(get("tokens_output"))
	reasoning, _ := intValue(get("tokens_reasoning"))
	cacheRead, _ := intValue(get("tokens_cache_read"))
	cacheWrite, _ := intValue(get("tokens_cache_write"))
	out += reasoning
	total := in + out + cacheRead + cacheWrite
	if total > 0 {
		setTokenFields(&s, in, out, cacheRead, cacheWrite, total)
	} else {
		s.TokensUsed = total
	}
	return s, true
}

func intValue(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return max(0, n), true
	case float64:
		return max(0, int64(n)), true
	}
	return 0, false
}

// opencodeLegacyInfo is one storage/session/info/*.json file.
type opencodeLegacyInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Model any    `json:"model"`
	Path  struct {
		CWD string `json:"cwd"`
	} `json:"path"`
	Directory string `json:"directory"`
	Version   string `json:"version"`
	Tokens    struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

func loadOpenCodeLegacy() []Session {
	infoDir := filepath.Join(opencodeDataHome(), "storage", "session", "info")
	matches, _ := filepath.Glob(filepath.Join(infoDir, "*.json"))
	var sessions []Session
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info opencodeLegacyInfo
		if json.Unmarshal(data, &info) != nil || info.ID == "" {
			continue
		}
		model, provider := opencodeModel(info.Model)
		cwd := info.Path.CWD
		if cwd == "" {
			cwd = info.Directory
		}
		created := normalizeEpoch(info.Time.Created)
		updated := normalizeEpoch(info.Time.Updated)
		if updated == 0 {
			updated = created
		}
		s := Session{
			ID:            info.ID,
			Title:         orEmpty(info.Title, "(无标题)"),
			CreatedAt:     created,
			UpdatedAt:     updated,
			CWD:           cwd,
			Model:         model,
			ModelProvider: provider,
			CliVersion:    info.Version,
			RolloutPath:   path,
			Tool:          ToolOpenCode,
		}
		out := info.Tokens.Output + info.Tokens.Reasoning
		total := info.Tokens.Input + out + info.Tokens.Cache.Read + info.Tokens.Cache.Write
		if total > 0 {
			setTokenFields(&s, info.Tokens.Input, out, info.Tokens.Cache.Read, info.Tokens.Cache.Write, total)
		}
		sessions = append(sessions, s)
	}
	return sessions
}

func orEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
