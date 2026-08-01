// Package search implements the Codex standalone web-search endpoint
// (POST {base_url}/alpha/search). When a custom provider opts into
// supports_standalone_web_search, Codex sends search requests here instead of
// to OpenAI's hosted search. This package proxies those requests to a
// third-party search backend (currently Exa).
//
// The request/response contract is defined by Codex's Rust source:
//
//	codex-rs/codex-api/src/search.rs (SearchRequest / SearchResponse)
//	codex-rs/codex-api/src/endpoint/search.rs (URL path: "alpha/search")
//	codex-rs/ext/web-search/src/tool.rs (invocation logic)
//
// Supported commands: search_query, open (URL only), find (URL only);
// image_query is served as a text search. All command groups in one request are
// executed and their output concatenated. Unsupported commands (click,
// screenshot, finance, weather, sports, time) are reported as skipped.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Codex search request types (mirrors of the Rust structs in search.rs) -------
// ---------------------------------------------------------------------------

// SearchRequest is the JSON body Codex POSTs to /alpha/search.
type SearchRequest struct {
	ID              string          `json:"id"`
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input,omitempty"` // conversation history (ignored)
	Commands        *SearchCommands `json:"commands,omitempty"`
	Settings        *SearchSettings `json:"settings,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
}

// SearchCommands carries the operations the LLM wants to perform. Only
// search_query, open, and find are acted upon; the rest are acknowledged.
type SearchCommands struct {
	SearchQuery    []SearchQuery   `json:"search_query,omitempty"`
	ImageQuery     []SearchQuery   `json:"image_query,omitempty"`
	Open           []OpenOperation `json:"open,omitempty"`
	Click          json.RawMessage `json:"click,omitempty"`
	Find           []FindOperation `json:"find,omitempty"`
	Screenshot     json.RawMessage `json:"screenshot,omitempty"`
	Finance        json.RawMessage `json:"finance,omitempty"`
	Weather        json.RawMessage `json:"weather,omitempty"`
	Sports         json.RawMessage `json:"sports,omitempty"`
	Time           json.RawMessage `json:"time,omitempty"`
	ResponseLength string          `json:"response_length,omitempty"`
}

// SearchQuery is a single web-search query.
type SearchQuery struct {
	Q       string   `json:"q"`
	Recency *int     `json:"recency,omitempty"` // filter by recent N days
	Domains []string `json:"domains,omitempty"` // restrict to these domains
}

// OpenOperation opens a page by URL or by ref_id from a previous search.
type OpenOperation struct {
	RefID  string `json:"ref_id"`
	Lineno *int   `json:"lineno,omitempty"`
}

// FindOperation searches for a text pattern inside a page.
type FindOperation struct {
	RefID   string `json:"ref_id"`
	Pattern string `json:"pattern"`
}

// SearchSettings carries optional search preferences.
type SearchSettings struct {
	UserLocation      json.RawMessage `json:"user_location,omitempty"`
	SearchContextSize string          `json:"search_context_size,omitempty"` // low/medium/high
	Filters           *SearchFilters  `json:"filters,omitempty"`
}

// SearchFilters restricts domains.
type SearchFilters struct {
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
}

// ---------------------------------------------------------------------------
// Codex search response types ------------------------------------------------
// ---------------------------------------------------------------------------

// SearchResponse is what Codex expects back from /alpha/search.
type SearchResponse struct {
	EncryptedOutput string         `json:"encrypted_output,omitempty"` // optional, can omit
	Output          string         `json:"output"`                     // required: text fed to the LLM
	Results         []SearchResult `json:"results,omitempty"`          // optional: structured results for UI
}

// SearchResult is one structured result item. Codex treats results as
// forward-compatible JSON (JsonValue in Rust), so extra fields are fine.
type SearchResult struct {
	Type    string `json:"type"`   // "text_result"
	RefID   string `json:"ref_id"` // e.g. "turn0search0"
	URL     string `json:"url"`
	Title   string `json:"title"`
	Snippet string `json:"snippet,omitempty"` // short excerpt
}

// ---------------------------------------------------------------------------
// Exa API types --------------------------------------------------------------
// ---------------------------------------------------------------------------

type exaSearchRequest struct {
	Query              string       `json:"query"`
	NumResults         int          `json:"numResults"`
	Type               string       `json:"type,omitempty"` // "neural" | "keyword" | "auto"
	IncludeDomains     []string     `json:"includeDomains,omitempty"`
	ExcludeDomains     []string     `json:"excludeDomains,omitempty"`
	StartPublishedDate string       `json:"startPublishedDate,omitempty"` // ISO date
	Contents           *exaContents `json:"contents,omitempty"`
}

// exaText mirrors Exa's `contents.text` object form. maxCharacters is nested
// inside text; sending it as a sibling of text makes Exa ignore it and return
// the whole page.
type exaText struct {
	MaxCharacters int `json:"maxCharacters,omitempty"`
}

type exaContents struct {
	Text exaText `json:"text"`
}

type exaSearchResponse struct {
	RequestID string      `json:"requestId"`
	Results   []exaResult `json:"results"`
}

type exaResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	PublishedDate string  `json:"publishedDate,omitempty"`
	Author        string  `json:"author,omitempty"`
	Score         float64 `json:"score,omitempty"`
	ID            string  `json:"id,omitempty"`
	Text          string  `json:"text,omitempty"`
}

type exaContentsRequest struct {
	IDs  []string `json:"ids"`
	Text exaText  `json:"text"`
}

type exaContentsResponse struct {
	Results []exaResult `json:"results"`
}

// ---------------------------------------------------------------------------
// Handler --------------------------------------------------------------------
// ---------------------------------------------------------------------------

// Handler implements http.Handler for the /alpha/search endpoint. It is nil-safe
// when search is not configured (the server simply does not register the route).
type Handler struct {
	client  *http.Client
	apiKey  string
	baseURL string
	log     *slog.Logger
}

// New creates a Handler for the Exa search backend.
func New(apiKey, baseURL string, log *slog.Logger) *Handler {
	if baseURL == "" {
		baseURL = "https://api.exa.ai"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		client:  &http.Client{Timeout: 30 * time.Second},
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		log:     log,
	}
}

// ServeHTTP handles POST /alpha/search.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errMap("failed to read request body: %s", err))
		return
	}

	var req SearchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap("invalid request body: %s", err))
		return
	}

	resp, status, apiErr := h.handleSearch(r.Context(), &req)
	if apiErr != nil {
		h.log.Error("search failed", "model", req.Model, "err", apiErr, "duration", time.Since(start).Round(time.Millisecond))
		writeJSON(w, status, errMap("%s", apiErr))
		return
	}

	h.log.Info("search", "model", req.Model, "results", len(resp.Results), "duration", time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusOK, resp)
}

const maxRequestBody = 4 << 20 // 4 MiB

// handleSearch runs every command group present in the request and concatenates
// their sections. Codex's web.run description tells the model to combine
// commands in a single call, so dispatching to just one would silently drop
// work. Returns the SearchResponse, an HTTP status code (0 on success), and an
// error.
func (h *Handler) handleSearch(ctx context.Context, req *SearchRequest) (*SearchResponse, int, error) {
	if req.Commands == nil {
		return &SearchResponse{Output: "No search commands provided."}, 0, nil
	}
	cmds := req.Commands

	var sections, failures []string
	var results []SearchResult
	var firstErr error
	run := func(fn func() (string, []SearchResult, error)) {
		text, res, err := fn()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			failures = append(failures, err.Error())
			return
		}
		if text != "" {
			sections = append(sections, text)
		}
		results = append(results, res...)
	}

	// search_query and image_query both map to Exa search.
	queries := append(append([]SearchQuery{}, cmds.SearchQuery...), cmds.ImageQuery...)
	if len(queries) > 0 {
		run(func() (string, []SearchResult, error) { return h.doSearch(ctx, req, queries) })
	}
	// open: fetch page contents by URL.
	if len(cmds.Open) > 0 {
		run(func() (string, []SearchResult, error) { return h.doOpen(ctx, cmds.Open) })
	}
	// find: fetch page and search for pattern.
	if len(cmds.Find) > 0 {
		run(func() (string, []SearchResult, error) { return h.doFind(ctx, cmds.Find) })
	}
	if note := unsupportedNote(cmds); note != "" {
		sections = append(sections, note)
	}

	// Nothing succeeded and something failed: report the upstream error rather
	// than an empty success.
	if len(sections) == 0 {
		if firstErr != nil {
			return nil, http.StatusBadGateway, firstErr
		}
		return &SearchResponse{Output: "No supported search commands provided."}, 0, nil
	}
	return &SearchResponse{
		Output:  strings.Join(append(sections, failures...), "\n\n"),
		Results: results,
	}, 0, nil
}

// unsupportedNote names the commands this backend cannot serve, so the model
// learns they were skipped instead of silently losing them.
func unsupportedNote(c *SearchCommands) string {
	var names []string
	for _, cmd := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"click", c.Click},
		{"screenshot", c.Screenshot},
		{"finance", c.Finance},
		{"weather", c.Weather},
		{"sports", c.Sports},
		{"time", c.Time},
	} {
		if len(cmd.raw) > 0 && string(cmd.raw) != "null" {
			names = append(names, cmd.name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Note: the following commands are not supported by this search backend and were skipped: %s.", strings.Join(names, ", "))
}

// doSearch calls Exa /search for each query (concurrently) and merges results.
func (h *Handler) doSearch(ctx context.Context, req *SearchRequest, queries []SearchQuery) (string, []SearchResult, error) {
	settings := req.Settings
	numResults, maxChars := contextSizeSettings(settings)

	type queryResult struct {
		query   SearchQuery
		results []exaResult
		err     error
	}

	results := make([]queryResult, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q SearchQuery) {
			defer wg.Done()
			results[i].query = q
			results[i].results, results[i].err = h.exaSearch(ctx, q, settings, numResults, maxChars)
		}(i, q)
	}
	wg.Wait()

	// Merge results from all queries, interleaving for diversity.
	var allResults []exaResult
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			h.log.Warn("exa search query failed", "query", r.query.Q, "err", r.err)
			continue
		}
		allResults = append(allResults, r.results...)
	}
	if firstErr != nil && len(allResults) == 0 {
		return "", nil, fmt.Errorf("exa search failed: %w", firstErr)
	}

	// Build output text and structured results.
	var b strings.Builder
	var searchResults []SearchResult
	for i, res := range allResults {
		refID := fmt.Sprintf("turn0search%d", i)
		snippet := truncate(res.Text, 300)

		searchResults = append(searchResults, SearchResult{
			Type:    "text_result",
			RefID:   refID,
			URL:     res.URL,
			Title:   res.Title,
			Snippet: snippet,
		})

		fmt.Fprintf(&b, "[%d] %s\n", i+1, res.Title)
		fmt.Fprintf(&b, "URL: %s\n", res.URL)
		if res.PublishedDate != "" {
			fmt.Fprintf(&b, "Published: %s\n", res.PublishedDate)
		}
		b.WriteString("\n")
		b.WriteString(res.Text)
		b.WriteString("\n\n---\n\n")
	}

	if b.Len() == 0 {
		return "No results found for the given queries.", nil, nil
	}

	return strings.TrimRight(b.String(), "\n"), searchResults, nil
}

// doOpen fetches full page content for URLs via Exa /contents.
func (h *Handler) doOpen(ctx context.Context, ops []OpenOperation) (string, []SearchResult, error) {
	var urls []string
	for _, op := range ops {
		if isURL(op.RefID) {
			urls = append(urls, op.RefID)
		}
	}
	if len(urls) == 0 {
		return "Open: no valid URL provided. Ref_id-based open is not supported.", nil, nil
	}

	exaResults, err := h.exaContents(ctx, urls, 8000)
	if err != nil {
		return "", nil, fmt.Errorf("exa contents failed: %w", err)
	}

	var b strings.Builder
	var searchResults []SearchResult
	for i, res := range exaResults {
		refID := fmt.Sprintf("turn0fetch%d", i)
		searchResults = append(searchResults, SearchResult{
			Type:    "text_result",
			RefID:   refID,
			URL:     res.URL,
			Title:   res.Title,
			Snippet: truncate(res.Text, 300),
		})

		fmt.Fprintf(&b, "[%d] %s\n", i+1, res.Title)
		fmt.Fprintf(&b, "URL: %s\n\n", res.URL)
		b.WriteString(res.Text)
		b.WriteString("\n\n---\n\n")
	}

	if b.Len() == 0 {
		return "No content found for the given URLs.", nil, nil
	}

	return strings.TrimRight(b.String(), "\n"), searchResults, nil
}

// doFind fetches a page and searches for a text pattern.
func (h *Handler) doFind(ctx context.Context, ops []FindOperation) (string, []SearchResult, error) {
	var b strings.Builder
	var searchResults []SearchResult

	for i, op := range ops {
		if !isURL(op.RefID) {
			fmt.Fprintf(&b, "[find] ref_id %q is not a URL; ref_id-based find is not supported.\n\n", op.RefID)
			continue
		}

		results, err := h.exaContents(ctx, []string{op.RefID}, 50000)
		if err != nil || len(results) == 0 {
			fmt.Fprintf(&b, "[find] Failed to fetch %s: %v\n\n", op.RefID, err)
			continue
		}

		text := results[0].Text
		matches := findPattern(text, op.Pattern)
		if len(matches) == 0 {
			fmt.Fprintf(&b, "[find] Pattern %q not found in %s.\n\n", op.Pattern, op.RefID)
			continue
		}

		refID := fmt.Sprintf("turn0find%d", i)
		snippet := strings.Join(matches, "\n...\n")
		searchResults = append(searchResults, SearchResult{
			Type:    "text_result",
			RefID:   refID,
			URL:     op.RefID,
			Title:   results[0].Title,
			Snippet: truncate(snippet, 300),
		})

		fmt.Fprintf(&b, "[find] %d match(es) for %q in %s:\n\n", len(matches), op.Pattern, op.RefID)
		b.WriteString(snippet)
		b.WriteString("\n\n---\n\n")
	}

	if b.Len() == 0 {
		return "No find results.", nil, nil
	}

	return strings.TrimRight(b.String(), "\n"), searchResults, nil
}

// ---------------------------------------------------------------------------
// Exa API calls --------------------------------------------------------------
// ---------------------------------------------------------------------------

func (h *Handler) exaSearch(ctx context.Context, q SearchQuery, settings *SearchSettings, numResults, maxChars int) ([]exaResult, error) {
	// Merge query-level domains with settings-level filters.
	includeDomains := uniqueStrings(append(append([]string{}, q.Domains...), domainFilter(settings, true)...))
	excludeDomains := domainFilter(settings, false)

	esr := exaSearchRequest{
		Query:          q.Q,
		NumResults:     numResults,
		Type:           "auto",
		IncludeDomains: includeDomains,
		ExcludeDomains: excludeDomains,
		Contents:       &exaContents{Text: exaText{MaxCharacters: maxChars}},
	}
	if q.Recency != nil && *q.Recency > 0 {
		esr.StartPublishedDate = time.Now().AddDate(0, 0, -*q.Recency).Format("2006-01-02")
	}

	var resp exaSearchResponse
	if err := h.postJSON(ctx, "/search", esr, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

func (h *Handler) exaContents(ctx context.Context, ids []string, maxChars int) ([]exaResult, error) {
	ecr := exaContentsRequest{
		IDs:  ids,
		Text: exaText{MaxCharacters: maxChars},
	}
	var resp exaContentsResponse
	if err := h.postJSON(ctx, "/contents", ecr, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

func (h *Handler) postJSON(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+path, strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.Header.Set("x-api-key", h.apiKey)

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("exa API %s: status %d: %s", path, resp.StatusCode, string(respBody))
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers --------------------------------------------------------------------
// ---------------------------------------------------------------------------

// contextSizeSettings maps Codex search_context_size to Exa numResults and
// maxCharacters.
func contextSizeSettings(s *SearchSettings) (numResults, maxChars int) {
	numResults, maxChars = 5, 2000 // defaults (medium)
	if s == nil {
		return
	}
	switch s.SearchContextSize {
	case "low":
		numResults, maxChars = 3, 1000
	case "high":
		numResults, maxChars = 10, 4000
	}
	return
}

// domainFilter extracts allowed (true) or blocked (false) domains from settings.
func domainFilter(s *SearchSettings, allowed bool) []string {
	if s == nil || s.Filters == nil {
		return nil
	}
	if allowed {
		return s.Filters.AllowedDomains
	}
	return s.Filters.BlockedDomains
}

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// findPattern searches for lines containing the pattern (case-insensitive).
func findPattern(text, pattern string) []string {
	if pattern == "" {
		return nil
	}
	lower := strings.ToLower(pattern)
	var matches []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(line), lower) {
			matches = append(matches, strings.TrimSpace(line))
			if len(matches) >= 20 {
				break
			}
		}
	}
	return matches
}

func uniqueStrings(s []string) []string {
	seen := make(map[string]bool, len(s))
	out := s[:0]
	for _, v := range s {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(format string, args ...any) map[string]string {
	return map[string]string{"error": fmt.Sprintf(format, args...)}
}
