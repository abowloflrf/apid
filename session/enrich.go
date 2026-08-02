package session

// Token enrichment. The listing readers only pull what is cheap: codex and
// opencode carry token columns in their databases, pi and claude need a full
// file pass. Enrichment is therefore lazy — the UI opts in per request — and
// cached per file keyed on mtime+size, so a stable transcript is scanned once
// per process run.

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

// EnrichCache memoizes per-file enrichment results. Safe for concurrent use.
type EnrichCache struct {
	mu    sync.Mutex
	items map[string]enrichEntry
}

type enrichEntry struct {
	mtime, size int64
	fields      usageFields
	known       bool // fields were actually found in the file
}

type usageFields struct {
	in, out, cacheRead, cacheWrite, total int64
}

// NewEnrichCache returns an empty cache.
func NewEnrichCache() *EnrichCache {
	return &EnrichCache{items: map[string]enrichEntry{}}
}

// Enrich fills token usage + cache hit rate for one session. opencode sessions
// already carry tokens from their db and need no pass.
func (c *EnrichCache) Enrich(s *Session) {
	switch s.Tool {
	case ToolCodex:
		c.enrichCodex(s)
	case ToolClaude:
		c.enrichClaude(s)
	case ToolPi:
		c.enrichPi(s)
	}
}

func (c *EnrichCache) cached(path string, mtime, size int64) (usageFields, bool) {
	if c == nil {
		return usageFields{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[path]
	if !ok || e.mtime != mtime || e.size != size {
		return usageFields{}, false
	}
	return e.fields, e.known
}

func (c *EnrichCache) store(path string, mtime, size int64, f usageFields) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items[path] = enrichEntry{mtime: mtime, size: size, fields: f, known: f.total > 0}
	c.mu.Unlock()
}

// fileKey returns the mtime+size fingerprint of a file.
func fileKey(path string) (mtime, size int64, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	return fi.ModTime().UnixNano(), fi.Size(), true
}

// ---- codex: rollout token_count ----

func (c *EnrichCache) enrichCodex(s *Session) {
	if s.CacheHitRate != nil {
		return // state db already carried the numbers
	}
	fields, ok := c.codexUsage(s.RolloutPath)
	if !ok {
		return
	}
	setTokenFields(s, fields.in, fields.out, fields.cacheRead, fields.cacheWrite, fields.total)
}

// codexUsage extracts the cumulative token usage from the last token_count
// event in the rollout file (the script shells out to rg for this; a plain
// scan is cheaper here).
func (c *EnrichCache) codexUsage(path string) (usageFields, bool) {
	if path == "" {
		return usageFields{}, false
	}
	if mtime, size, ok := fileKey(path); ok {
		if f, known := c.cached(path, mtime, size); known {
			return f, true
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return usageFields{}, false
	}
	defer f.Close()

	var last usageFields
	var found bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Payload struct {
				Type string `json:"type"`
				Info struct {
					TotalTokenUsage map[string]any `json:"total_token_usage"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type != "token_count" && ev.Payload.Type != "token_count" {
			continue
		}
		fields := usageFieldsFrom(ev.Payload.Info.TotalTokenUsage)
		if fields.valid() {
			last = fields
			found = true
		}
	}
	if err := sc.Err(); err != nil || !found {
		return usageFields{}, false
	}
	if mtime, size, ok := fileKey(path); ok {
		c.store(path, mtime, size, last)
	}
	return last, true
}

// ---- claude: sum usage across assistant messages ----

func (c *EnrichCache) enrichClaude(s *Session) {
	path := s.RolloutPath
	if path == "" {
		return
	}
	if mtime, size, ok := fileKey(path); ok {
		if f, known := c.cached(path, mtime, size); known {
			setTokenFields(s, f.in, f.out, f.cacheRead, f.cacheWrite, f.total)
			return
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var in, out, cacheRead, cacheWrite int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens   int64 `json:"input_tokens"`
					OutputTokens  int64 `json:"output_tokens"`
					CacheRead     int64 `json:"cache_read_input_tokens"`
					CacheCreation int64 `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.Type != "assistant" {
			continue
		}
		u := line.Message.Usage
		in += max(0, u.InputTokens)
		out += max(0, u.OutputTokens)
		cacheRead += max(0, u.CacheRead)
		cacheWrite += max(0, u.CacheCreation)
	}
	fields := usageFields{in: in, out: out, cacheRead: cacheRead, cacheWrite: cacheWrite}
	fields.total = in + out + cacheRead + cacheWrite
	if mtime, size, ok := fileKey(path); ok {
		c.store(path, mtime, size, fields)
	}
	if fields.total > 0 {
		setTokenFields(s, in, out, cacheRead, cacheWrite, fields.total)
	}
}

// ---- pi: sum usage on assistant/toolResult messages and summaries ----

func (c *EnrichCache) enrichPi(s *Session) {
	path := s.RolloutPath
	if path == "" {
		return
	}
	if mtime, size, ok := fileKey(path); ok {
		if f, known := c.cached(path, mtime, size); known {
			setTokenFields(s, f.in, f.out, f.cacheRead, f.cacheWrite, f.total)
			return
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var in, out, cacheRead, cacheWrite, total int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Message *struct {
				Role  string         `json:"role"`
				Usage map[string]any `json:"usage"`
			} `json:"message"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		var usage map[string]any
		switch line.Type {
		case "message":
			if m := line.Message; m != nil && (m.Role == "assistant" || m.Role == "toolResult") {
				usage = m.Usage
			}
		case "branch_summary", "compaction":
			usage = line.Usage
		}
		if usage == nil {
			continue
		}
		f := usageFieldsFrom(usage)
		in += f.in
		out += f.out
		cacheRead += f.cacheRead
		cacheWrite += f.cacheWrite
		total += f.total
	}
	fields := usageFields{in: in, out: out, cacheRead: cacheRead, cacheWrite: cacheWrite, total: total}
	if mtime, size, ok := fileKey(path); ok {
		c.store(path, mtime, size, fields)
	}
	if total > 0 {
		setTokenFields(s, in, out, cacheRead, cacheWrite, total)
	}
}

// usageFieldsFrom normalizes a usage object across the field-name variants the
// four tools emit (openai-style *_tokens vs the short camelCase forms).
func usageFieldsFrom(usage map[string]any) usageFields {
	var f usageFields
	f.in = num(usage["input_tokens"], usage["input"])
	f.out = num(usage["output_tokens"], usage["output"])
	f.cacheRead = num(usage["cached_input_tokens"], usage["cacheRead"])
	f.cacheWrite = num(usage["cache_write_input_tokens"], usage["cacheWrite"])
	if cache, ok := usage["cache"].(map[string]any); ok {
		f.cacheRead = max(f.cacheRead, num(cache["read"]))
		f.cacheWrite = max(f.cacheWrite, num(cache["write"]))
	}
	f.total = num(usage["total_tokens"], usage["totalTokens"])
	if f.total == 0 {
		f.total = f.in + f.out + f.cacheRead + f.cacheWrite
	}
	return f
}

func (f usageFields) valid() bool { return f.total > 0 }

func num(vals ...any) int64 {
	for _, v := range vals {
		switch n := v.(type) {
		case float64:
			return max(0, int64(n))
		case int64:
			return max(0, n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return max(0, i)
			}
		}
	}
	return 0
}
