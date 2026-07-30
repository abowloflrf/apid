package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/store"
)

// forwardConfig builds a same-protocol (pure-forward) config with one upstream.
func forwardConfig(path string, proto config.Protocol, upstreamURL, upstreamPath, model string) config.Config {
	return config.Config{
		Upstreams: []config.Upstream{{
			Name:     "up",
			Protocol: proto,
			BaseURL:  upstreamURL,
			Path:     upstreamPath,
			Model:    model,
		}},
		Routes: []config.Route{{
			Path:          path,
			InputProtocol: proto,
			Models:        []config.ModelRule{{Match: "*", Upstream: "up"}},
		}},
	}
}

// TestForwardChatNonStream：chat->chat 纯转发非流式，断言字节原样透传 + usage 提取。
func TestForwardChatNonStream(t *testing.T) {
	const upstreamBody = `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":3}}}`
	var gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := readAll(r)
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "f.db")
	st, _ := store.Open(dbPath)
	defer st.Close()
	srv := New(forwardConfig("/v1/chat/completions", config.ProtoChat, up.URL, "/v1/chat/completions", ""), st, nil)
	defer srv.Close()

	reqBody := `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != upstreamBody {
		t.Errorf("响应未原样透传:\n got=%s\nwant=%s", rec.Body.String(), upstreamBody)
	}
	if gotBody != reqBody {
		t.Errorf("请求未原样转发:\n got=%s\nwant=%s", gotBody, reqBody)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("上游 path = %q", gotPath)
	}

	var cProto, upProto, cModel, upModel, upURL string
	var inTok, outTok, totalTok, cachedTok, stream int
	if err := st.DB().QueryRow(`SELECT client_protocol, upstream_protocol, client_model, upstream_model,
		upstream_url, input_tokens, output_tokens, total_tokens, cached_tokens, stream FROM requests LIMIT 1`).
		Scan(&cProto, &upProto, &cModel, &upModel, &upURL, &inTok, &outTok, &totalTok, &cachedTok, &stream); err != nil {
		t.Fatal(err)
	}
	if cProto != "openai_chat_completions" || upProto != "openai_chat_completions" {
		t.Errorf("protocol 错: client=%q upstream=%q", cProto, upProto)
	}
	if cModel != "gpt-x" || upModel != "gpt-x" {
		t.Errorf("model 错: client=%q upstream=%q", cModel, upModel)
	}
	if inTok != 7 || outTok != 4 || totalTok != 11 || cachedTok != 3 {
		t.Errorf("token 错: in=%d out=%d total=%d cached=%d, want 7/4/11/3", inTok, outTok, totalTok, cachedTok)
	}
	if stream != 0 {
		t.Errorf("stream = %d, want 0", stream)
	}
	if !strings.HasSuffix(upURL, "/v1/chat/completions") {
		t.Errorf("upstream_url = %q", upURL)
	}
}

// TestForwardResponsesNonStream：responses->responses 纯转发，断言 Responses usage 形状提取。
func TestForwardResponsesNonStream(t *testing.T) {
	const upstreamBody = `{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":9,"output_tokens":6,"total_tokens":15,"input_tokens_details":{"cached_tokens":4}}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer up.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "f.db"))
	defer st.Close()
	srv := New(forwardConfig("/v1/responses", config.ProtoResponses, up.URL, "/v1/responses", ""), st, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-x","input":"hi"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	if rec.Code != http.StatusOK || rec.Body.String() != upstreamBody {
		t.Fatalf("响应不对: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var inTok, outTok, totalTok, cachedTok int
	if err := st.DB().QueryRow(`SELECT input_tokens, output_tokens, total_tokens, cached_tokens
		FROM requests LIMIT 1`).Scan(&inTok, &outTok, &totalTok, &cachedTok); err != nil {
		t.Fatal(err)
	}
	if inTok != 9 || outTok != 6 || totalTok != 15 || cachedTok != 4 {
		t.Errorf("token 错: in=%d out=%d total=%d cached=%d, want 9/6/15/4", inTok, outTok, totalTok, cachedTok)
	}
}

func TestForwardAnthropicMessagesNonStream(t *testing.T) {
	const upstreamBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":9,"cache_creation_input_tokens":2,"cache_read_input_tokens":4,"output_tokens":6}}`
	var gotPath, gotBody, gotKey, gotVersion string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		b, _ := readAll(r)
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer up.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "f.db"))
	defer st.Close()
	cfg := forwardConfig("/v1/messages", config.ProtoAnthropic, up.URL, "/v1/messages", "")
	cfg.Upstreams[0].APIKey = "sk-ant-test"
	srv := New(cfg, st, nil)
	defer srv.Close()

	reqBody := `{"model":"claude-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	if rec.Code != http.StatusOK || rec.Body.String() != upstreamBody {
		t.Fatalf("响应不对: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" || gotBody != reqBody {
		t.Errorf("上游请求不对: path=%q body=%s", gotPath, gotBody)
	}
	if gotKey != "sk-ant-test" {
		t.Errorf("X-Api-Key = %q, want configured key", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want forwarded", gotVersion)
	}

	var cProto, upProto string
	var inTok, outTok, totalTok, cachedTok int
	if err := st.DB().QueryRow(`SELECT client_protocol, upstream_protocol, input_tokens, output_tokens, total_tokens, cached_tokens
		FROM requests LIMIT 1`).Scan(&cProto, &upProto, &inTok, &outTok, &totalTok, &cachedTok); err != nil {
		t.Fatal(err)
	}
	if cProto != "anthropic_messages" || upProto != "anthropic_messages" {
		t.Errorf("protocol 错: client=%q upstream=%q", cProto, upProto)
	}
	if inTok != 15 || outTok != 6 || totalTok != 21 || cachedTok != 4 {
		t.Errorf("token 错: in=%d out=%d total=%d cached=%d, want 15/6/21/4", inTok, outTok, totalTok, cachedTok)
	}
}

func TestClientAPIKeyAuthProtectsForwardRoute(t *testing.T) {
	var hitUpstream bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitUpstream = true
		if v := r.Header.Get("Authorization"); v != "" {
			t.Errorf("Authorization leaked upstream: %q", v)
		}
		if v := r.Header.Get("X-Api-Key"); v != "" {
			t.Errorf("X-Api-Key leaked upstream: %q", v)
		}
		if v := r.Header.Get("Api-Key"); v != "" {
			t.Errorf("Api-Key leaked upstream: %q", v)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cfg := forwardConfig("/v1/messages", config.ProtoAnthropic, up.URL, "/v1/messages", "")
	cfg.ClientAPIKey = "apid-key"
	srv := New(cfg, nil, nil)
	defer srv.Close()
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", rec.Code)
	}
	if hitUpstream {
		t.Fatal("upstream should not be hit without client API key")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	req.Header.Set("Authorization", "Bearer apid-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !hitUpstream {
		t.Fatal("upstream should be hit with valid client API key")
	}
}

func TestClientAPIKeyAuthAcceptsAnthropicStyleHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, header, value string
	}{
		{"x api key", "X-Api-Key", "apid-key"},
		{"api key", "Api-Key", "apid-key"},
		{"authorization bearer", "Authorization", "Bearer apid-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer up.Close()

			cfg := forwardConfig("/v1/messages", config.ProtoAnthropic, up.URL, "/v1/messages", "")
			cfg.ClientAPIKey = "apid-key"
			srv := New(cfg, nil, nil)
			defer srv.Close()

			req := httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestClientAPIKeyNotLeakedWhenUpstreamHasConfiguredAnthropicKey(t *testing.T) {
	var gotAuth, gotXAPIKey, gotAPIKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("X-Api-Key")
		gotAPIKey = r.Header.Get("Api-Key")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cfg := forwardConfig("/v1/messages", config.ProtoAnthropic, up.URL, "/v1/messages", "")
	cfg.ClientAPIKey = "apid-key"
	cfg.Upstreams[0].APIKey = "real-anthropic-key"
	srv := New(cfg, nil, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	req.Header.Set("X-Api-Key", "apid-key")
	req.Header.Set("Authorization", "Bearer should-not-leak")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotXAPIKey != "real-anthropic-key" {
		t.Errorf("upstream X-Api-Key = %q, want configured upstream key", gotXAPIKey)
	}
	if gotAuth != "" || gotAPIKey != "" {
		t.Errorf("client auth leaked upstream: Authorization=%q Api-Key=%q", gotAuth, gotAPIKey)
	}
}

func TestForwardRawPreservesQuery(t *testing.T) {
	var gotRawQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "f.db"))
	defer st.Close()
	srv := New(forwardConfig("/v1/messages", config.ProtoAnthropic, up.URL, "/v1/messages", ""), st, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true&x=1",
		strings.NewReader(`{"model":"claude-sonnet","messages":[]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotRawQuery != "beta=true&x=1" {
		t.Errorf("上游 query = %q, want beta=true&x=1", gotRawQuery)
	}
	var upURL string
	if err := st.DB().QueryRow(`SELECT upstream_url FROM requests LIMIT 1`).Scan(&upURL); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(upURL, "/v1/messages?beta=true&x=1") {
		t.Errorf("upstream_url = %q, want query preserved", upURL)
	}
}

// TestForwardChatStream：chat->chat 纯转发流式，断言 SSE 原样 + usage + TTFT>0 + stream=1。
func TestForwardChatStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"content":"he"}}]}`,
			`{"choices":[{"delta":{"content":"llo"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "f.db"))
	defer st.Close()
	srv := New(forwardConfig("/v1/chat/completions", config.ProtoChat, up.URL, "/v1/chat/completions", ""), st, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	out := rec.Body.String()
	if !strings.Contains(out, `data: {"choices":[{"delta":{"content":"he"}}]}`) {
		t.Errorf("SSE 未原样透传: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("缺 [DONE]: %s", out)
	}

	var stream, inTok, totalTok int
	var ttft *int
	if err := st.DB().QueryRow(`SELECT stream, input_tokens, total_tokens, ttft_ms
		FROM requests LIMIT 1`).Scan(&stream, &inTok, &totalTok, &ttft); err != nil {
		t.Fatal(err)
	}
	if stream != 1 {
		t.Errorf("stream = %d, want 1", stream)
	}
	if inTok != 2 || totalTok != 4 {
		t.Errorf("usage 错: in=%d total=%d, want 2/4", inTok, totalTok)
	}
	if ttft == nil || *ttft < 0 {
		t.Errorf("ttft_ms 应有非负值, got %v", ttft)
	}
}

func TestForwardAnthropicMessagesStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunks := []string{
			`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":1,"cache_read_input_tokens":2,"output_tokens":0}}}
`,
			`event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"he"}}
`,
			`event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":3}}
`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c + "\n"))
			fl.Flush()
		}
	}))
	defer up.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "f.db"))
	defer st.Close()
	srv := New(forwardConfig("/v1/messages", config.ProtoAnthropic, up.URL, "/v1/messages", ""), st, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	out := rec.Body.String()
	if !strings.Contains(out, `event: content_block_delta`) || !strings.Contains(out, `"text":"he"`) {
		t.Errorf("Anthropic SSE 未原样透传: %s", out)
	}

	var stream, inTok, outTok, totalTok, cachedTok int
	var ttft *int
	if err := st.DB().QueryRow(`SELECT stream, input_tokens, output_tokens, total_tokens, cached_tokens, ttft_ms
		FROM requests LIMIT 1`).Scan(&stream, &inTok, &outTok, &totalTok, &cachedTok, &ttft); err != nil {
		t.Fatal(err)
	}
	if stream != 1 {
		t.Errorf("stream = %d, want 1", stream)
	}
	if inTok != 8 || outTok != 3 || totalTok != 11 || cachedTok != 2 {
		t.Errorf("usage 错: in=%d out=%d total=%d cached=%d, want 8/3/11/2", inTok, outTok, totalTok, cachedTok)
	}
	if ttft == nil || *ttft < 0 {
		t.Errorf("ttft_ms 应有非负值, got %v", ttft)
	}
}

// TestForwardModelOverride：纯转发配 upstream_model 时，转发给上游的 model 被改写。
func TestForwardModelOverride(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := readAll(r)
		_ = json.Unmarshal([]byte(b), &m)
		gotModel, _ = m["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	srv := New(forwardConfig("/v1/chat/completions", config.ProtoChat, up.URL, "/v1/chat/completions", "backend-model"), nil, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-x","messages":[]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if gotModel != "backend-model" {
		t.Errorf("上游收到 model = %q, want backend-model", gotModel)
	}
}

// TestForwardUpstreamError：纯转发上游 4xx，错误体透传 + stats.error 非空。
func TestForwardUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer up.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "f.db"))
	defer st.Close()
	srv := New(forwardConfig("/v1/chat/completions", config.ProtoChat, up.URL, "/v1/chat/completions", ""), st, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("错误体未透传: %s", rec.Body.String())
	}
	var cStatus, upStatus int
	var errStr *string
	if err := st.DB().QueryRow(`SELECT client_status, upstream_status, error FROM requests LIMIT 1`).
		Scan(&cStatus, &upStatus, &errStr); err != nil {
		t.Fatal(err)
	}
	if cStatus != 429 || upStatus != 429 {
		t.Errorf("status 错: client=%d upstream=%d", cStatus, upStatus)
	}
	if errStr == nil || !strings.Contains(*errStr, "rate limited") {
		t.Errorf("error 字段应含上游错误, got %v", errStr)
	}
}

// TestRouteByPath: multiple routes dispatch to their own upstream by path;
// unregistered path returns 404.
func TestRouteByPath(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"from":"A"}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"from":"B"}`))
	}))
	defer upB.Close()

	srv := New(config.Config{
		Upstreams: []config.Upstream{
			{Name: "a", Protocol: config.ProtoChat, BaseURL: upA.URL, Path: "/v1/chat/completions"},
			{Name: "b", Protocol: config.ProtoChat, BaseURL: upB.URL, Path: "/v1/chat/completions"},
		},
		Routes: []config.Route{
			{Path: "/a/chat", InputProtocol: config.ProtoChat, Models: []config.ModelRule{{Match: "*", Upstream: "a"}}},
			{Path: "/b/chat", InputProtocol: config.ProtoChat, Models: []config.ModelRule{{Match: "*", Upstream: "b"}}},
		},
	}, nil, nil)
	defer srv.Close()
	h := srv.Handler()

	for _, tc := range []struct{ path, want string }{
		{"/a/chat", "A"}, {"/b/chat", "B"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"m"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s hit wrong upstream: %s", tc.path, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/unknown", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unregistered path status = %d, want 404", rec.Code)
	}
}

// TestRouteByModel: one path, different models dispatch to different upstreams;
// an unroutable model returns 400.
func TestRouteByModel(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"from":"A"}`))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"from":"B"}`))
	}))
	defer upB.Close()

	srv := New(config.Config{
		Upstreams: []config.Upstream{
			{Name: "a", Protocol: config.ProtoChat, BaseURL: upA.URL, Path: "/v1/chat/completions"},
			{Name: "b", Protocol: config.ProtoChat, BaseURL: upB.URL, Path: "/v1/chat/completions"},
		},
		Routes: []config.Route{{
			Path:          "/v1/chat/completions",
			InputProtocol: config.ProtoChat,
			Models: []config.ModelRule{
				{Match: "model-a", Upstream: "a"},
				{Match: "b-*", Upstream: "b"},
			},
		}},
	}, nil, nil)
	defer srv.Close()
	h := srv.Handler()

	for _, tc := range []struct{ model, want string }{
		{"model-a", "A"}, {"b-large", "B"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"`+tc.model+`"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("model %q hit wrong upstream: %s", tc.model, rec.Body.String())
		}
	}

	// No catch-all, so an unknown model is unroutable.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"unknown"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unroutable model status = %d, want 400", rec.Code)
	}
}

func readAll(r *http.Request) (string, error) {
	b, err := io.ReadAll(r.Body)
	return string(b), err
}

// okSink writes successfully and discards; used to drive forwardSSE read paths.
type okSink struct{}

func (okSink) Write(p []byte) (int, error) { return len(p), nil }
func (okSink) Flush()                      {}

// failSink fails on Write, simulating a disconnected client.
type failSink struct{}

func (failSink) Write([]byte) (int, error) { return 0, fmt.Errorf("client gone") }
func (failSink) Flush()                    {}

// errReader yields one chunk then a non-EOF error mid-stream.
type errReader struct{ sent bool }

func (e *errReader) Read(p []byte) (int, error) {
	if e.sent {
		return 0, fmt.Errorf("conn reset")
	}
	e.sent = true
	return copy(p, []byte("data: {}\n")), nil
}

// TestForwardSSEWriteError: a failing client write stops forwarding and returns error.
func TestForwardSSEWriteError(t *testing.T) {
	_, _, err := forwardSSE(context.Background(), failSink{}, strings.NewReader("data: {}\n\n"), config.ProtoChat, nil)
	if err == nil {
		t.Fatal("expected write error, got nil")
	}
}

// TestForwardSSEReadError: a non-EOF upstream read error is returned (not swallowed).
func TestForwardSSEReadError(t *testing.T) {
	_, _, err := forwardSSE(context.Background(), okSink{}, &errReader{}, config.ProtoChat, nil)
	if err == nil {
		t.Fatal("expected read error, got nil")
	}
}

// TestForwardSSECleanEOF: a normal stream ending in EOF returns no error.
func TestForwardSSECleanEOF(t *testing.T) {
	_, _, err := forwardSSE(context.Background(), okSink{}, strings.NewReader("data: {}\n\ndata: [DONE]\n\n"), config.ProtoChat, nil)
	if err != nil {
		t.Fatalf("clean EOF should not error, got %v", err)
	}
}

// TestForwardErrorContentType: pure-forward preserves the upstream error Content-Type.
func TestForwardErrorContentType(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer up.Close()

	srv := New(forwardConfig("/v1/chat/completions", config.ProtoChat, up.URL, "/v1/chat/completions", ""), nil, nil)
	defer srv.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain passthrough", ct)
	}
	if rec.Body.String() != "slow down" {
		t.Errorf("body = %q, want verbatim", rec.Body.String())
	}
}
