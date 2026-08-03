package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/protocol"
	"github.com/abowloflrf/apid/store"
)

// mockUpstream 返回一个模拟 Chat Completions 的上游服务器。
func mockUpstream(t *testing.T, stream bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 校验请求确实被转换成了 chat 格式。
		var req protocol.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("上游收到的请求无法解析: %v", err)
		}
		if len(req.Messages) == 0 {
			t.Errorf("上游收到的 messages 为空")
		}

		if !stream {
			_ = json.NewEncoder(w).Encode(protocol.ChatResponse{
				ID: "chatcmpl-abc", Object: "chat.completion", Created: 1, Model: req.Model,
				Choices: []protocol.ChatChoice{{
					Message: protocol.ChatMessage{Role: "assistant", Content: "你好世界"}, FinishReason: "stop",
				}},
				Usage: &protocol.ChatUsage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, tok := range []string{"你", "好", "世界"} {
			chunk := protocol.ChatStreamChunk{
				ID: "chatcmpl-abc", Model: req.Model,
				Choices: []protocol.ChatStreamChoice{{Delta: protocol.ChatChunkDelta{Content: tok}}},
			}
			b, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
}

// convertRoute builds a responses -> chat convert config with one upstream.
func convertRoute(upstreamURL, model string) config.Config {
	return config.Config{
		Upstreams: []config.Upstream{{
			Name:     "up",
			Protocol: config.ProtoChat,
			BaseURL:  upstreamURL,
			Path:     "/chat/completions",
			Model:    model,
		}},
		Routes: []config.Route{{
			Path:          "/v1/responses",
			InputProtocol: config.ProtoResponses,
			Models:        []config.ModelRule{{Match: "*", Upstream: "up"}},
		}},
	}
}

func newTestServer(upstreamURL string) http.Handler {
	return New(convertRoute(upstreamURL, ""), nil, nil).Handler()
}

func TestNonStreaming(t *testing.T) {
	up := mockUpstream(t, false)
	defer up.Close()
	h := newTestServer(up.URL)

	body := `{"model":"gpt-x","instructions":"你是助手","input":"你好"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp protocol.ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Errorf("响应顶层字段不对: %+v", resp)
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 {
		t.Fatalf("output 结构不对: %+v", resp.Output)
	}
	if got := resp.Output[0].Content[0].Text; got != "你好世界" {
		t.Errorf("文本 = %q, 期望 你好世界", got)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 8 {
		t.Errorf("usage 不对: %+v", resp.Usage)
	}
}

// TestUpstreamModelOverride 验证配置 UpstreamModel 后，转发给上游的 model 被覆盖。
func TestUpstreamModelOverride(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		_ = json.NewEncoder(w).Encode(protocol.ChatResponse{
			ID: "chatcmpl-abc", Object: "chat.completion", Created: 1, Model: req.Model,
			Choices: []protocol.ChatChoice{{Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer up.Close()

	h := New(convertRoute(up.URL, "real-backend-model"), nil, nil).Handler()

	body := `{"model":"gpt-x","input":"你好"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "real-backend-model" {
		t.Errorf("上游收到的 model = %q, 期望 real-backend-model", gotModel)
	}
}

// TestEffectiveModel checks rule override precedence over the upstream default.
func TestEffectiveModel(t *testing.T) {
	rewrite := "rule-model"
	empty := ""
	up := config.Upstream{Model: "upstream-model"}
	cases := []struct {
		name string
		rule config.ModelRule
		want string
	}{
		{"rule rewrite wins", config.ModelRule{Model: &rewrite}, "rule-model"},
		{"rule empty forces passthrough", config.ModelRule{Model: &empty}, ""},
		{"nil inherits upstream", config.ModelRule{}, "upstream-model"},
	}
	for _, tc := range cases {
		if got := effectiveModel(tc.rule, up); got != tc.want {
			t.Errorf("%s: effectiveModel = %q; want %q", tc.name, got, tc.want)
		}
	}
}

// TestRuleModelOverride 验证 [[route.model]] 上的 model 覆盖了 upstream 的默认 model。
func TestRuleModelOverride(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		_ = json.NewEncoder(w).Encode(protocol.ChatResponse{
			ID: "chatcmpl-abc", Object: "chat.completion", Created: 1, Model: req.Model,
			Choices: []protocol.ChatChoice{{Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer up.Close()

	ruleModel := "rule-level-model"
	cfg := convertRoute(up.URL, "upstream-default")
	cfg.Routes[0].Models[0].Model = &ruleModel // rule override should win
	h := New(cfg, nil, nil).Handler()

	body := `{"model":"gpt-x","input":"你好"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != ruleModel {
		t.Errorf("上游收到的 model = %q, 期望 %q", gotModel, ruleModel)
	}
}

// TestRuleModelPassthrough 验证 rule model 为空串时，即便 upstream 配了默认，也透传客户端 model。
func TestRuleModelPassthrough(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		_ = json.NewEncoder(w).Encode(protocol.ChatResponse{
			ID: "chatcmpl-abc", Object: "chat.completion", Created: 1, Model: req.Model,
			Choices: []protocol.ChatChoice{{Message: protocol.ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer up.Close()

	passthrough := ""
	cfg := convertRoute(up.URL, "upstream-default")
	cfg.Routes[0].Models[0].Model = &passthrough // explicit passthrough beats upstream default
	h := New(cfg, nil, nil).Handler()

	body := `{"model":"gpt-x","input":"你好"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "gpt-x" {
		t.Errorf("上游收到的 model = %q, 期望透传 gpt-x", gotModel)
	}
}

func TestStreaming(t *testing.T) {
	up := mockUpstream(t, true)
	defer up.Close()
	h := newTestServer(up.URL)

	body := `{"model":"gpt-x","input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}

	var events []string
	var fullText strings.Builder
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			events = append(events, strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
				if m["type"] == "response.output_text.delta" {
					fullText.WriteString(m["delta"].(string))
				}
			}
		}
	}

	if events[0] != "response.created" {
		t.Errorf("首个事件 = %q, 期望 response.created", events[0])
	}
	if last := events[len(events)-1]; last != "response.completed" {
		t.Errorf("末个事件 = %q, 期望 response.completed", last)
	}
	if fullText.String() != "你好世界" {
		t.Errorf("拼接文本 = %q, 期望 你好世界", fullText.String())
	}
}

var _ = io.Discard

// TestStatsRecorded 端到端验证：一次成功的非流式请求经由 server 处理后，
// 指标应被 stats 包异步写入 SQLite，并能查回到关键字段。
func TestStatsRecorded(t *testing.T) {
	up := mockUpstream(t, false)
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "apid.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(convertRoute(up.URL, ""), st, nil)
	defer srv.Close()

	body := `{"model":"gpt-x","input":"你好","client_metadata":{"session_id":"flat-session","thread_id":"codex-thread","x-codex-turn-metadata":"{\"session_id\":\"codex-session\"}"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("session-id", "header-session")
	req.Header.Set("thread-id", "codex-thread")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}

	// srv.Close() 在 defer 触发前不会跑，先显式关一次让 worker 把 channel 排空。
	srv.Close()

	row := st.DB().QueryRow(`SELECT
		client_model, upstream_url, upstream_model, COALESCE(session_id, ''), stream,
		client_status, upstream_status,
		input_tokens, output_tokens, total_tokens, cached_tokens
		FROM requests LIMIT 1`)
	var (
		cModel, upURL, upModel, sessionID                             string
		stream, cStatus, upStatus, inTok, outTok, totalTok, cachedTok int
	)
	if err := row.Scan(&cModel, &upURL, &upModel, &sessionID, &stream, &cStatus, &upStatus,
		&inTok, &outTok, &totalTok, &cachedTok); err != nil {
		t.Fatalf("read back row failed: %v", err)
	}
	if cModel != "gpt-x" {
		t.Errorf("client_model = %q, want gpt-x", cModel)
	}
	if upModel != "gpt-x" {
		t.Errorf("upstream_model = %q, want gpt-x (未配置覆盖时透传)", upModel)
	}
	if sessionID != "codex-session" {
		t.Errorf("session_id = %q, want codex-session", sessionID)
	}
	if !strings.HasSuffix(upURL, "/chat/completions") {
		t.Errorf("upstream_url = %q, 应以 /chat/completions 结尾", upURL)
	}
	if stream != 0 {
		t.Errorf("stream = %d, want 0", stream)
	}
	if cStatus != http.StatusOK {
		t.Errorf("client_status = %d, want 200", cStatus)
	}
	if upStatus != http.StatusOK {
		t.Errorf("upstream_status = %d, want 200", upStatus)
	}
	if inTok != 5 || outTok != 3 || totalTok != 8 {
		t.Errorf("tokens 错: in=%d out=%d total=%d, want 5/3/8", inTok, outTok, totalTok)
	}
}

// TestStatsUpstreamError 端到端验证：上游返回 4xx 时，stats 仍记录一行，error 字段非空。
func TestStatsUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer up.Close()

	dbPath := filepath.Join(t.TempDir(), "apid.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	srv := New(convertRoute(up.URL, ""), st, nil)
	defer srv.Close()

	body := `{"model":"m","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	srv.Close()

	var clientStatus, upstreamStatus int
	var errStr *string
	if err := st.DB().QueryRow(
		`SELECT client_status, upstream_status, error FROM requests LIMIT 1`,
	).Scan(&clientStatus, &upstreamStatus, &errStr); err != nil {
		t.Fatal(err)
	}
	if clientStatus != http.StatusBadRequest {
		t.Errorf("client_status = %d, want 400", clientStatus)
	}
	if upstreamStatus != http.StatusBadRequest {
		t.Errorf("upstream_status = %d, want 400", upstreamStatus)
	}
	if errStr == nil || !strings.Contains(*errStr, "bad request") {
		t.Errorf("error 字段应包含上游错误信息, got %v", errStr)
	}
}
