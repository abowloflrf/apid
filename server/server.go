// Package server 提供对外的 HTTP 服务：按配置的多条路由分派，
// 同协议纯转发、异协议做协议转换，全程采集请求指标。
package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/convert"
	"github.com/abowloflrf/apid/protocol"
	"github.com/abowloflrf/apid/reasoning"
	"github.com/abowloflrf/apid/stats"
	"github.com/abowloflrf/apid/store"
	"github.com/abowloflrf/apid/trace"
	"github.com/abowloflrf/apid/upstream"
)

// route is a config entrypoint keyed by its exposed path.
type route struct {
	cfg config.Route
}

// target is a resolved upstream: its config plus a bound forwarding client,
// shared across every route/model rule that references it.
type target struct {
	cfg    config.Upstream
	client *upstream.Client
}

type Server struct {
	routes    map[string]*route
	upstreams map[string]*target
	log       *slog.Logger
	tracer    *trace.Tracer
	recorder  *stats.Recorder
	db        *sql.DB          // read-only handle for the stats query API; nil when storage is off
	reasoning *reasoning.Cache // in-memory reasoning_content cache for multi-turn round-trip
	apiKey    string           // optional inbound client API key; empty = no client auth
}

// New builds a Server. st may be nil (stats disabled); a nil logger falls back
// to slog.Default(). Each upstream gets one shared client; tracer/recorder are
// global.
func New(cfg config.Config, st *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	upstreams := make(map[string]*target, len(cfg.Upstreams))
	for _, uc := range cfg.Upstreams {
		upstreams[uc.Name] = &target{
			cfg:    uc,
			client: upstream.New(uc.BaseURL, uc.Path, uc.APIKey, upstreamOptions(uc.Protocol)...),
		}
	}
	routes := make(map[string]*route, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		routes[rc.Path] = &route{cfg: rc}
	}
	var db *sql.DB
	if st.Enabled() {
		db = st.DB()
	}
	return &Server{
		routes:    routes,
		upstreams: upstreams,
		log:       logger,
		tracer:    trace.New(cfg.TraceDir, logger),
		recorder:  stats.NewRecorder(st, 0, logger),
		db:        db,
		reasoning: reasoning.New(reasoning.DefaultCapacity, reasoning.DefaultTTL),
		apiKey:    cfg.ClientAPIKey,
	}
}

func upstreamOptions(proto config.Protocol) []upstream.Option {
	if proto == config.ProtoAnthropic {
		return []upstream.Option{upstream.WithXAPIKeyAuth()}
	}
	return nil
}

func (s *Server) resolve(rt *route, model string) (*target, config.ModelRule) {
	rule, ok := rt.cfg.Resolve(model)
	if !ok {
		return nil, config.ModelRule{}
	}
	return s.upstreams[rule.Upstream], rule
}

func effectiveModel(rule config.ModelRule, up config.Upstream) string {
	if rule.Model != nil {
		return *rule.Model
	}
	return up.Model
}

func (s *Server) Close() {
	if s == nil {
		return
	}
	s.recorder.Close()
}

// Handler 返回配置好路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes {
		mux.HandleFunc("POST "+rt.cfg.Path, func(w http.ResponseWriter, r *http.Request) {
			s.handleRoute(rt, w, r)
		})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /stats/daily", s.handleStatsDaily)
	// Interactive dashboard: JSON API + the embedded UI. The specific /stats/*
	// API patterns outrank the /stats/ subtree, so only the page falls through
	// to the file server.
	mux.HandleFunc("GET /stats/summary", s.handleStatsSummary)
	mux.HandleFunc("GET /stats/by_model", s.handleStatsByModel)
	mux.HandleFunc("GET /stats/timeseries", s.handleStatsTimeSeries)
	mux.HandleFunc("GET /stats/requests", s.handleStatsRequests)
	mux.HandleFunc("GET /stats/options", s.handleStatsOptions)
	mux.Handle("GET /stats/", s.statsUIHandler())
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/stats/", http.StatusMovedPermanently)
	})
	return s.withClientAuth(mux)
}

// handleRoute is the shared orchestration: read body, sniff model, resolve the
// target upstream, trace, collect stats, then dispatch to convert or raw forward.
func (s *Server) handleRoute(rt *route, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &respRecorder{ResponseWriter: w}

	stat := stats.Record{
		Time:           start,
		ClientProtocol: string(rt.cfg.InputProtocol),
		ClientPath:     r.URL.RequestURI(),
		ClientUA:       r.UserAgent(),
	}
	defer func() {
		stat.Duration = time.Since(start)
		stat.ClientStatus = rec.statusCode()
		s.recorder.Record(stat)
	}()
	defer func() {
		attrs := []any{
			"method", r.Method, "path", r.URL.Path,
			"model", stat.ClientModel, "stream", stat.Stream,
			"upstream_url", stat.UpstreamURL, "upstream_model", stat.UpstreamModel,
			"upstream_status", stat.UpstreamStatus, "status", rec.statusCode(),
			"duration", time.Since(start).Round(time.Millisecond),
		}
		if u := stat.Usage; u != nil {
			attrs = append(attrs, slog.Group("usage",
				"input", u.InputTokens, "output", u.OutputTokens, "total", u.TotalTokens,
				"cache_read", u.CachedTokens, "cache_creation", u.CacheCreationTokens))
		}
		s.log.Info("access", attrs...)
	}()

	upstreamReq := r
	if s.apiKey != "" {
		upstreamReq = r.Clone(r.Context())
		upstreamReq.Header = upstreamClientHeaders(r.Header)
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		stat.Error = "read request body: " + err.Error()
		writeError(rec, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}

	var sniff reqSniff
	_ = json.Unmarshal(bodyBytes, &sniff)
	stat.ClientModel = sniff.Model
	stat.Stream = sniff.Stream

	tg, rule := s.resolve(rt, sniff.Model)
	if tg == nil {
		stat.Error = "no upstream for model " + sniff.Model
		writeError(rec, http.StatusBadRequest, "no upstream configured for model "+strconv.Quote(sniff.Model))
		return
	}
	effModel := effectiveModel(rule, tg.cfg)
	stat.UpstreamProtocol = string(tg.cfg.Protocol)
	stat.UpstreamURL = tg.client.Endpoint()
	if rt.cfg.InputProtocol == tg.cfg.Protocol {
		stat.UpstreamURL = tg.client.EndpointWithQuery(r.URL.RawQuery)
	}
	stat.UpstreamModel = sniff.Model
	if effModel != "" {
		stat.UpstreamModel = effModel
	}

	traceEntry := s.tracer.Begin(r.Method, r.URL.RequestURI())
	if path := traceEntry.Dump(string(rt.cfg.InputProtocol), bodyBytes); path != "" {
		s.log.Info("trace request", "file", path)
	}

	// Same protocol on both ends => raw forward; otherwise convert.
	if rt.cfg.InputProtocol == tg.cfg.Protocol {
		s.forwardRaw(tg, effModel, rec, upstreamReq, bodyBytes, traceEntry, &stat, start)
		return
	}
	s.convertResponsesToChat(tg, effModel, rec, upstreamReq, bodyBytes, traceEntry, &stat, start)
}

func (s *Server) authorizeClient(w http.ResponseWriter, r *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	if clientKeyMatches(r.Header, s.apiKey) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="apid"`)
	writeError(w, http.StatusUnauthorized, "invalid or missing API key")
	return false
}

func (s *Server) withClientAuth(next http.Handler) http.Handler {
	if s.apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExemptPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authorizeClient(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authExemptPath reports whether a request bypasses client auth: the health
// probe and the read-only stats/dashboard endpoints (GET /stats and /stats/*),
// so the embedded dashboard and Grafana keep working. Only the forwarding
// routes (POST) require the client key.
func authExemptPath(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	return p == "/healthz" || p == "/stats" || strings.HasPrefix(p, "/stats/")
}

// clientKeyMatches accepts the common API-key header styles used by OpenAI-like
// and Anthropic clients: Authorization: Bearer <key>, X-Api-Key, and Api-Key.
func clientKeyMatches(h http.Header, want string) bool {
	if want == "" {
		return true
	}
	for _, got := range clientAPIKeyCandidates(h) {
		if constantTimeStringEqual(got, want) {
			return true
		}
	}
	return false
}

func clientAPIKeyCandidates(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Authorization") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if len(v) >= 7 && strings.EqualFold(v[:7], "Bearer ") {
			if token := strings.TrimSpace(v[7:]); token != "" {
				out = append(out, token)
			}
			continue
		}
		out = append(out, v)
	}
	for _, key := range []string{"X-Api-Key", "Api-Key"} {
		for _, v := range h.Values(key) {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// upstreamClientHeaders removes the credentials that authenticated the client
// to apid before the request is sent upstream. This prevents an apid-level key
// from becoming an accidental upstream key when an upstream.api_key is omitted.
func upstreamClientHeaders(src http.Header) http.Header {
	dst := src.Clone()
	for _, key := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key"} {
		dst.Del(key)
	}
	return dst
}

// convertResponsesToChat handles a responses -> chat route.
func (s *Server) convertResponsesToChat(tg *target, effModel string, w http.ResponseWriter, r *http.Request, bodyBytes []byte, traceEntry *trace.Entry, stat *stats.Record, start time.Time) {
	var req protocol.ResponsesRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		stat.Error = "parse request body: " + err.Error()
		writeError(w, http.StatusBadRequest, "failed to parse request body: "+err.Error())
		return
	}

	chatReq, namespaces, err := convert.ResponsesToChat(&req, s.reasoning)
	if err != nil {
		stat.Error = "convert request: " + err.Error()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 修正 system/developer 消息顺序：Codex 可能把 developer 插在
	// function_call 与 function_call_output 之间，移到 messages[0]。
	chatReq.Messages = convert.FixSystemOrdering(chatReq.Messages)

	if effModel != "" {
		chatReq.Model = effModel
	}
	stat.UpstreamModel = chatReq.Model

	if traceEntry != nil {
		if chatBytes, err := json.Marshal(chatReq); err != nil {
			s.log.Error("trace marshal chat request failed", "err", err)
		} else if path := traceEntry.Dump("chat", chatBytes); path != "" {
			s.log.Info("trace chat request", "file", path)
		}
	}

	resp, err := tg.client.ChatCompletions(r.Context(), chatReq, r.Header)
	if err != nil {
		stat.Error = "upstream: " + err.Error()
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	stat.UpstreamStatus = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.passUpstreamError(w, resp, stat)
		return
	}

	if req.Stream {
		s.streamResponse(r.Context(), w, resp.Body, req.Model, namespaces, stat, start)
		return
	}
	s.jsonResponse(w, resp.Body, namespaces, stat)
}

// ---- 非流式 ----

func (s *Server) jsonResponse(w http.ResponseWriter, body io.Reader, namespaces map[string]convert.NamespacedTool, stat *stats.Record) {
	var chatResp protocol.ChatResponse
	if err := json.NewDecoder(body).Decode(&chatResp); err != nil {
		stat.Error = "parse upstream response: " + err.Error()
		writeError(w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}
	stat.Usage = toStatsUsage(chatResp.Usage)

	out := convert.ChatToResponses(&chatResp, namespaces)

	// 回写 reasoning 缓存，供下轮重放回填。
	if len(chatResp.Choices) > 0 {
		a := chatResp.Choices[0].Message
		s.saveReasoning(toolCallIDs(a.ToolCalls), a.Content, a.ReasoningContent)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.log.Warn("write response failed", "err", err)
	}
}

// ---- 流式 ----

// streamResponse 处理流式：把上游 SSE 转换为 Responses 事件流，完成后回写 reasoning 缓存。
func (s *Server) streamResponse(ctx context.Context, w http.ResponseWriter, body io.Reader, model string, namespaces map[string]convert.NamespacedTool, stat *stats.Record, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		stat.Error = "streaming not supported"
		writeError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sw := &sseWriter{w: w, f: flusher}
	result, err := convert.StreamChatToResponses(ctx, sw, body, model, namespaces)
	if err != nil {
		s.log.Warn("stream conversion failed", "err", err)
		stat.Error = "stream conversion: " + err.Error()
	}
	if result != nil {
		if result.Usage != nil {
			stat.Usage = toStatsUsage(result.Usage)
		}
		if !result.FirstTokenAt.IsZero() {
			stat.TTFT = result.FirstTokenAt.Sub(start)
		}
		// 回写 reasoning 缓存，供下轮重放回填。
		s.saveReasoning(result.ToolCallIDs, result.ContentText, result.ReasoningText)
	}
}

// saveReasoning 把本轮 assistant 的 reasoning_content 写入缓存：按每个 tool
// call_id、以及按 assistant 文本指纹各存一份，供下轮重放时回填。空值自动忽略。
func (s *Server) saveReasoning(callIDs []string, content, reasoning string) {
	if reasoning == "" {
		return
	}
	for _, id := range callIDs {
		s.reasoning.SaveCall(id, reasoning)
	}
	s.reasoning.SaveContent(content, reasoning)
}

// toolCallIDs 提取 assistant.tool_calls 里的 call_id 列表。
func toolCallIDs(calls []protocol.ChatToolCall) []string {
	if len(calls) == 0 {
		return nil
	}
	ids := make([]string, 0, len(calls))
	for _, tc := range calls {
		if tc.ID != "" {
			ids = append(ids, tc.ID)
		}
	}
	return ids
}

// ---- 通用辅助 ----

func (s *Server) passUpstreamError(w http.ResponseWriter, resp *http.Response, stat *stats.Record) {
	errBody, _ := io.ReadAll(resp.Body)
	s.log.Warn("upstream error", "status", resp.StatusCode, "body", string(errBody))
	stat.Error = truncate(string(errBody), 512)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
}

func toStatsUsage(u *protocol.ChatUsage) *stats.Usage {
	if u == nil {
		return nil
	}
	out := &stats.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// respRecorder 包装 http.ResponseWriter，记录最终写出的状态码用于 access log。
type respRecorder struct {
	http.ResponseWriter
	status int
}

func (r *respRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *respRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *respRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s *sseWriter) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s *sseWriter) Flush()                      { s.f.Flush() }

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "apid_error"},
	})
}
