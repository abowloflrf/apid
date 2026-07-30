package search

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockExa starts a test server that mocks the Exa API and records requests.
type mockExa struct {
	*httptest.Server
	searchReq   []byte
	contentsReq []byte
}

func newMockExa(t *testing.T) *mockExa {
	t.Helper()
	m := &mockExa{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/search":
			m.searchReq = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"requestId": "req-1",
				"results": [
					{
						"title": "Go Programming Language",
						"url": "https://go.dev",
						"publishedDate": "2024-01-15",
						"text": "Go is a statically typed, compiled programming language."
					},
					{
						"title": "Rust Programming Language",
						"url": "https://rust-lang.org",
						"publishedDate": "2024-02-01",
						"text": "Rust is a systems programming language that runs blazingly fast."
					}
				]
			}`))
		case "/contents":
			m.contentsReq = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"results": [
					{
						"title": "Example Page",
						"url": "https://example.com/page",
						"text": "This is the full page content. It contains important information about the topic."
					}
				]
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func TestSearchQuery(t *testing.T) {
	mock := newMockExa(t)
	h := New("test-key", mock.URL, nil)

	reqBody := `{
		"id": "session-1",
		"model": "gpt-test",
		"commands": {
			"search_query": [{"q": "programming languages"}]
		},
		"settings": {"search_context_size": "low"}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Output == "" {
		t.Error("expected non-empty output")
	}
	if !strings.Contains(resp.Output, "Go Programming Language") {
		t.Errorf("output should contain result title, got: %s", resp.Output)
	}
	if !strings.Contains(resp.Output, "https://go.dev") {
		t.Errorf("output should contain URL, got: %s", resp.Output)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Results[0].RefID != "turn0search0" {
		t.Errorf("expected ref_id turn0search0, got %s", resp.Results[0].RefID)
	}
	if resp.Results[0].URL != "https://go.dev" {
		t.Errorf("unexpected URL: %s", resp.Results[0].URL)
	}

	// Verify Exa received the right request.
	var exaReq exaSearchRequest
	if err := json.Unmarshal(mock.searchReq, &exaReq); err != nil {
		t.Fatalf("decode exa request: %v", err)
	}
	if exaReq.Query != "programming languages" {
		t.Errorf("expected query 'programming languages', got %q", exaReq.Query)
	}
	if exaReq.NumResults != 3 { // low = 3
		t.Errorf("expected 3 results for low context, got %d", exaReq.NumResults)
	}
	// maxCharacters must sit inside contents.text; as a sibling Exa ignores it
	// and returns whole pages.
	if exaReq.Contents == nil || exaReq.Contents.Text.MaxCharacters != 1000 {
		t.Errorf("expected contents.text.maxCharacters = 1000, got %+v", exaReq.Contents)
	}
	var raw struct {
		Contents struct {
			Text map[string]any `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(mock.searchReq, &raw); err != nil {
		t.Fatalf("decode exa request shape: %v", err)
	}
	if got := raw.Contents.Text["maxCharacters"]; got != float64(1000) {
		t.Errorf("contents.text must be an object carrying maxCharacters, got %v", raw.Contents.Text)
	}
	if exaReq.Type != "auto" {
		t.Errorf("expected type 'auto', got %q", exaReq.Type)
	}
}

func TestSearchQueryWithFilters(t *testing.T) {
	mock := newMockExa(t)
	h := New("test-key", mock.URL, nil)

	reqBody := `{
		"id": "session-1",
		"model": "gpt-test",
		"commands": {
			"search_query": [{"q": "rust", "recency": 7, "domains": ["rust-lang.org"]}]
		},
		"settings": {
			"search_context_size": "high",
			"filters": {
				"allowed_domains": ["rust-lang.org"],
				"blocked_domains": ["example.com"]
			}
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var exaReq exaSearchRequest
	if err := json.Unmarshal(mock.searchReq, &exaReq); err != nil {
		t.Fatalf("decode exa request: %v", err)
	}
	if exaReq.NumResults != 10 { // high = 10
		t.Errorf("expected 10 results for high context, got %d", exaReq.NumResults)
	}
	if exaReq.StartPublishedDate == "" {
		t.Error("expected non-empty startPublishedDate for recency filter")
	}
	// Domains from both query and settings should be merged.
	if len(exaReq.IncludeDomains) != 1 || exaReq.IncludeDomains[0] != "rust-lang.org" {
		t.Errorf("expected includeDomains [rust-lang.org], got %v", exaReq.IncludeDomains)
	}
	if len(exaReq.ExcludeDomains) != 1 || exaReq.ExcludeDomains[0] != "example.com" {
		t.Errorf("expected excludeDomains [example.com], got %v", exaReq.ExcludeDomains)
	}
}

func TestMultipleSearchQueries(t *testing.T) {
	mock := newMockExa(t)
	h := New("test-key", mock.URL, nil)

	reqBody := `{
		"id": "session-1",
		"model": "gpt-test",
		"commands": {
			"search_query": [{"q": "golang"}, {"q": "rust"}]
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Each query returns 2 results, so merged = 4.
	if len(resp.Results) != 4 {
		t.Fatalf("expected 4 results (2 queries x 2 results), got %d", len(resp.Results))
	}
}

func TestOpenURL(t *testing.T) {
	mock := newMockExa(t)
	h := New("test-key", mock.URL, nil)

	reqBody := `{
		"id": "session-1",
		"model": "gpt-test",
		"commands": {
			"open": [{"ref_id": "https://example.com/page"}]
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp.Output, "Example Page") {
		t.Errorf("output should contain page title, got: %s", resp.Output)
	}
	if !strings.Contains(resp.Output, "full page content") {
		t.Errorf("output should contain page text, got: %s", resp.Output)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].RefID != "turn0fetch0" {
		t.Errorf("expected ref_id turn0fetch0, got %s", resp.Results[0].RefID)
	}

	// Verify Exa /contents was called with the URL.
	var exaReq exaContentsRequest
	if err := json.Unmarshal(mock.contentsReq, &exaReq); err != nil {
		t.Fatalf("decode exa contents request: %v", err)
	}
	if len(exaReq.IDs) != 1 || exaReq.IDs[0] != "https://example.com/page" {
		t.Errorf("unexpected ids: %v", exaReq.IDs)
	}
	if exaReq.Text.MaxCharacters != 8000 {
		t.Errorf("expected text.maxCharacters = 8000, got %d", exaReq.Text.MaxCharacters)
	}
}

// Codex's web.run description tells the model to combine commands in one call,
// so every group must run instead of only the first.
func TestCombinedCommands(t *testing.T) {
	mock := newMockExa(t)
	h := New("test-key", mock.URL, nil)

	reqBody := `{
		"id": "session-1",
		"model": "gpt-test",
		"commands": {
			"search_query": [{"q": "golang"}],
			"open": [{"ref_id": "https://example.com/page"}],
			"find": [{"ref_id": "https://example.com/page", "pattern": "important"}],
			"time": [{"utc_offset": "+03:00"}]
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	for _, want := range []string{
		"Go Programming Language", // search_query
		"Example Page",            // open
		"[find] 1 match(es)",      // find
		"not supported",           // time
	} {
		if !strings.Contains(resp.Output, want) {
			t.Errorf("output missing %q, got:\n%s", want, resp.Output)
		}
	}
	// 2 search results + 1 open + 1 find.
	if len(resp.Results) != 4 {
		t.Errorf("expected 4 merged results, got %d", len(resp.Results))
	}
}

// A failing command group must not swallow the sections that succeeded.
func TestCombinedCommandsPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/contents" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": "boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results": [{"title": "T", "url": "https://t.example", "text": "body"}]}`))
	}))
	defer srv.Close()

	h := New("test-key", srv.URL, nil)
	reqBody := `{
		"id": "s1",
		"model": "m1",
		"commands": {
			"search_query": [{"q": "golang"}],
			"open": [{"ref_id": "https://example.com/page"}]
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp.Output, "https://t.example") {
		t.Errorf("successful search section should survive, got:\n%s", resp.Output)
	}
	if !strings.Contains(resp.Output, "exa contents failed") {
		t.Errorf("failed section should be reported, got:\n%s", resp.Output)
	}
}

func TestFindPattern(t *testing.T) {
	mock := newMockExa(t)
	h := New("test-key", mock.URL, nil)

	reqBody := `{
		"id": "session-1",
		"model": "gpt-test",
		"commands": {
			"find": [{"ref_id": "https://example.com/page", "pattern": "important"}]
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp.Output, "important") {
		t.Errorf("output should contain the pattern match, got: %s", resp.Output)
	}
}

func TestNoCommands(t *testing.T) {
	h := New("test-key", "https://api.exa.ai", nil)

	reqBody := `{"id": "s1", "model": "m1"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Output == "" {
		t.Error("expected non-empty output for no commands")
	}
}

func TestInvalidBody(t *testing.T) {
	h := New("test-key", "https://api.exa.ai", nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader("not json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestExaError(t *testing.T) {
	// Mock server that returns an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "invalid api key"}`))
	}))
	defer srv.Close()

	h := New("bad-key", srv.URL, nil)

	reqBody := `{
		"id": "s1",
		"model": "m1",
		"commands": {"search_query": [{"q": "test"}]}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIsURL(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"https://example.com", true},
		{"http://localhost:8080/path", true},
		{"turn0search0", false},
		{"not a url", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isURL(c.input); got != c.want {
			t.Errorf("isURL(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

func TestContextSizeSettings(t *testing.T) {
	cases := []struct {
		size     string
		wantNum  int
		wantChar int
	}{
		{"low", 3, 1000},
		{"medium", 5, 2000},
		{"high", 10, 4000},
		{"", 5, 2000}, // default
	}
	for _, c := range cases {
		s := &SearchSettings{SearchContextSize: c.size}
		num, char := contextSizeSettings(s)
		if num != c.wantNum || char != c.wantChar {
			t.Errorf("size=%q: got (%d, %d), want (%d, %d)", c.size, num, char, c.wantNum, c.wantChar)
		}
	}
	// nil settings should use defaults.
	num, char := contextSizeSettings(nil)
	if num != 5 || char != 2000 {
		t.Errorf("nil settings: got (%d, %d), want (5, 2000)", num, char)
	}
}
