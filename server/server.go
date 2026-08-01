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
	"github.com/abowloflrf/apid/search"
	"github.com/abowloflrf/apid/stats"
	"github.com/abowloflrf/apid/store"
	"github.com/abowloflrf/apid/trace"
	"github.com/abowloflrf/apid/upstream"
)

// nonStreamTimeout caps the total duration of a non-streaming upstream
// request (connect + send + wait-for-headers + read-body). Streaming
// requests are exempt because they may stay open for many minutes (see
// newTransport).
const nonStreamTimeout = 300 * time.Second

// target is a resolved upstream: its config plus a bound forwarding client,
// shared across every route/model rule that references it.
type target struct {
	cfg    config.Upstream
	client *upstream.Client
}

// exchange bundles the per-request state threaded through the two forwarding
// paths (raw forward / protocol conversion).
type exchange struct {
	target       *target
	proto        config.Protocol     // protocol actually spoken upstream (wire protocol)
	viaResponses bool                // raw-forward to the upstream's dedicated Responses endpoint
	model        string              // effective model override; "" = pass the client model through
	w            http.ResponseWriter // wraps the client connection; records status for the access log
	req          *http.Request       // inbound request; auth headers already scrubbed when client auth is on
	body         []byte
	trace        *trace.Entry
	stat         *stats.Record
	live         *liveRequest // in-flight entry for the live dashboard; nil-safe
	start        time.Time
}

type Server struct {
	cfg             config.Config // as loaded, kept for the read-only topology endpoint
	routes          map[string]config.Route
	upstreams       map[string]*target
	log             *slog.Logger
	tracer          *trace.Tracer
	recorder        *stats.Recorder
	db              *sql.DB          // read-only handle for the stats query API; nil when storage is off
	reasoning       *reasoning.Cache // in-memory reasoning_content cache for multi-turn round-trip
	apiKey          string           // optional inbound client API key; empty = no client auth
	search          *search.Handler  // nil when [search] is not configured
	searchPathField string           // mount path for search endpoint; "" = not configured
	live            *liveRegistry    // in-flight requests, for GET /stats/active
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
		opts := upstreamOptions(uc.Protocol)
		if uc.SupportsResponses {
			opts = append(opts, upstream.WithResponsesPath(uc.EffectiveResponsesPath()))
		}
		upstreams[uc.Name] = &target{
			cfg:    uc,
			client: upstream.New(uc.BaseURL, uc.Path, uc.APIKey, opts...),
		}
	}
	routes := make(map[string]config.Route, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		routes[rc.Path] = rc
	}
	var db *sql.DB
	if st.Enabled() {
		db = st.DB()
	}
	return &Server{
		cfg:             cfg,
		routes:          routes,
		upstreams:       upstreams,
		log:             logger,
		tracer:          trace.New(cfg.TraceDir, logger),
		recorder:        stats.NewRecorder(st, 0, logger),
		db:              db,
		reasoning:       reasoning.New(reasoning.DefaultCapacity, reasoning.DefaultTTL),
		apiKey:          cfg.ClientAPIKey,
		search:          newSearchHandler(cfg.Search, logger),
		searchPathField: searchPathFromConfig(cfg.Search),
		live:            newLiveRegistry(),
	}
}

// newSearchHandler builds a search.Handler when [search] is configured,
// or returns nil when search is disabled.
func newSearchHandler(sc config.SearchConfig, log *slog.Logger) *search.Handler {
	if sc.Provider == "" {
		return nil
	}
	log.Info("search enabled", "provider", sc.Provider, "path", searchPathFromConfig(sc))
	return search.New(sc.APIKey, sc.BaseURL, log)
}

// searchPathFromConfig returns the configured search endpoint path, defaulting
// to /v1/alpha/search when [search].path is empty.
func searchPathFromConfig(sc config.SearchConfig) string {
	if sc.Path != "" {
		return sc.Path
	}
	return "/v1/alpha/search"
}

func upstreamOptions(proto config.Protocol) []upstream.Option {
	if proto == config.ProtoAnthropic {
		return []upstream.Option{upstream.WithXAPIKeyAuth()}
	}
	return nil
}

func (s *Server) resolve(rt config.Route, model string) (*target, config.ModelRule) {
	rule, ok := rt.Resolve(model)
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

// searchPath returns the path the search endpoint is mounted on. It defaults
// to /v1/alpha/search but can be overridden via [search].path in config.
func (s *Server) searchPath() string {
	return s.searchPathField
}

// Handler 返回配置好路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes {
		mux.HandleFunc("POST "+rt.Path, func(w http.ResponseWriter, r *http.Request) {
			s.handleRoute(rt, w, r)
		})
	}
	if s.search != nil {
		path := s.searchPath()
		mux.HandleFunc("POST "+path, s.search.ServeHTTP)
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
	// Config topology: read-only view of the loaded routes/upstreams. Unlike the
	// stats API it works without storage, since it reads config, not the DB.
	mux.HandleFunc("GET /stats/topology", s.handleTopology)
	// Live in-flight requests: in-memory registry, also storage-independent.
	mux.HandleFunc("GET /stats/active", s.handleStatsActive)
	mux.Handle("GET /stats/", s.statsUIHandler())
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		// Relative redirect so it still lands on /stats/ when apid is mounted
		// under a reverse-proxy subpath; an absolute "/stats/" would drop the
		// prefix. The browser resolves it against the request URL it used.
		w.Header().Set("Location", "stats/")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	return s.withClientAuth(mux)
}

// handleRoute is the shared orchestration: read body, sniff model, resolve the
// target upstream, trace, collect stats, then dispatch to convert or raw forward.
func (s *Server) handleRoute(rt config.Route, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &respRecorder{ResponseWriter: w}

	stat := stats.Record{
		Time:           start,
		ClientProtocol: string(rt.InputProtocol),
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

	bodyBytes, err := readLimited(r.Body, maxRequestBody)
	if err != nil {
		if err == errBodyTooLarge {
			stat.Error = "request body exceeds size limit"
			writeError(rec, http.StatusRequestEntityTooLarge, "request body exceeds size limit")
			return
		}
		stat.Error = "read request body: " + err.Error()
		writeError(rec, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}

	var sniff reqSniff
	_ = json.Unmarshal(bodyBytes, &sniff)
	stat.ClientModel = sniff.Model
	stat.Stream = sniff.Stream

	// Non-streaming requests get a hard cap so a hung upstream can't pin a
	// goroutine forever. Streaming requests stay uncapped (see newTransport).
	if !sniff.Stream {
		ctx, cancel := context.WithTimeout(upstreamReq.Context(), nonStreamTimeout)
		defer cancel()
		upstreamReq = upstreamReq.WithContext(ctx)
	}

	tg, rule := s.resolve(rt, sniff.Model)
	if tg == nil {
		stat.Error = "no upstream for model " + sniff.Model
		writeError(rec, http.StatusBadRequest, "no upstream configured for model "+strconv.Quote(sniff.Model))
		return
	}
	effModel := effectiveModel(rule, tg.cfg)
	stat.UpstreamModel = sniff.Model
	if effModel != "" {
		stat.UpstreamModel = effModel
	}

	// A chat upstream with supports_responses also serves the Responses protocol
	// natively: a responses route then forwards raw bytes to its Responses
	// endpoint instead of going through responses -> chat conversion. The wire
	// protocol seen upstream is openai_responses, not the configured primary.
	wireProto := tg.cfg.Protocol
	viaResponses := false
	if rt.InputProtocol == config.ProtoResponses && tg.cfg.Protocol == config.ProtoChat && tg.cfg.SupportsResponses {
		wireProto = config.ProtoResponses
		viaResponses = true
	}
	stat.UpstreamProtocol = string(wireProto)
	switch {
	case viaResponses:
		stat.UpstreamURL = tg.client.ResponsesEndpointWithQuery(r.URL.RawQuery)
	case rt.InputProtocol == wireProto:
		stat.UpstreamURL = tg.client.EndpointWithQuery(r.URL.RawQuery)
	default:
		stat.UpstreamURL = tg.client.Endpoint()
	}

	traceEntry := s.tracer.Begin(r.Method, r.URL.RequestURI())
	if path := traceEntry.Dump(string(rt.InputProtocol), bodyBytes); path != "" {
		s.log.Info("trace request", "file", path)
	}

	live := s.live.begin(liveBegin{
		clientModel:   sniff.Model,
		upstreamModel: stat.UpstreamModel,
		clientProto:   string(rt.InputProtocol),
		upstreamProto: string(wireProto),
		upstream:      rule.Upstream,
		path:          rt.Path,
		ua:            r.UserAgent(),
		stream:        sniff.Stream,
		inputEstimate: len(bodyBytes) / 4, // ~4 bytes per token; replaced once usage arrives
	})
	defer live.end()

	ex := &exchange{
		target:       tg,
		proto:        wireProto,
		viaResponses: viaResponses,
		model:        effModel,
		w:            rec,
		req:          upstreamReq,
		body:         bodyBytes,
		trace:        traceEntry,
		stat:         &stat,
		live:         live,
		start:        start,
	}
	// Same protocol on both ends (including responses -> responses via a
	// dual-protocol upstream) => raw forward; otherwise convert.
	if rt.InputProtocol == wireProto {
		s.forwardRaw(ex)
		return
	}
	s.convertResponsesToChat(ex)
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
func (s *Server) convertResponsesToChat(ex *exchange) {
	var req protocol.ResponsesRequest
	if err := json.Unmarshal(ex.body, &req); err != nil {
		ex.stat.Error = "parse request body: " + err.Error()
		writeError(ex.w, http.StatusBadRequest, "failed to parse request body: "+err.Error())
		return
	}

	chatReq, namespaces, err := convert.ResponsesToChat(&req, s.reasoning)
	if err != nil {
		ex.stat.Error = "convert request: " + err.Error()
		writeError(ex.w, http.StatusBadRequest, err.Error())
		return
	}

	// 修正 system/developer 消息顺序：Codex 可能把 developer 插在
	// function_call 与 function_call_output 之间，移到 messages[0]。
	chatReq.Messages = convert.FixSystemOrdering(chatReq.Messages)

	if ex.model != "" {
		chatReq.Model = ex.model
	}
	ex.stat.UpstreamModel = chatReq.Model

	if ex.trace != nil {
		if chatBytes, err := json.Marshal(chatReq); err != nil {
			s.log.Error("trace marshal chat request failed", "err", err)
		} else if path := ex.trace.Dump("chat", chatBytes); path != "" {
			s.log.Info("trace chat request", "file", path)
		}
	}

	resp, err := ex.target.client.ChatCompletions(ex.req.Context(), chatReq, ex.req.Header)
	if err != nil {
		ex.stat.Error = "upstream: " + err.Error()
		writeError(ex.w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	ex.stat.UpstreamStatus = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.passUpstreamError(ex.w, resp, ex.stat)
		return
	}

	if req.Stream {
		s.streamResponse(ex, resp.Body, req.Model, namespaces)
		return
	}
	s.jsonResponse(ex, resp.Body, namespaces)
}

// ---- 非流式 ----

func (s *Server) jsonResponse(ex *exchange, body io.Reader, namespaces map[string]convert.NamespacedTool) {
	var chatResp protocol.ChatResponse
	if err := json.NewDecoder(body).Decode(&chatResp); err != nil {
		ex.stat.Error = "parse upstream response: " + err.Error()
		writeError(ex.w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}
	ex.stat.Usage = toStatsUsage(chatResp.Usage)

	out := convert.ChatToResponses(&chatResp, namespaces)

	// 回写 reasoning 缓存，供下轮重放回填。
	if len(chatResp.Choices) > 0 {
		a := chatResp.Choices[0].Message
		s.saveReasoning(toolCallIDs(a.ToolCalls), a.Content, a.ReasoningContent)
	}

	s.writeJSON(ex.w, out)
}

// ---- 流式 ----

// streamResponse 处理流式：把上游 SSE 转换为 Responses 事件流，完成后回写 reasoning 缓存。
// clientModel 是回显给客户端的 model（Responses 响应回显请求里的 model）。
func (s *Server) streamResponse(ex *exchange, body io.Reader, clientModel string, namespaces map[string]convert.NamespacedTool) {
	flusher, ok := ex.w.(http.Flusher)
	if !ok {
		ex.stat.Error = "streaming not supported"
		writeError(ex.w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}
	ex.w.Header().Set("Content-Type", "text/event-stream")
	ex.w.Header().Set("Cache-Control", "no-cache")
	ex.w.Header().Set("Connection", "keep-alive")
	ex.w.WriteHeader(http.StatusOK)

	sw := &sseWriter{w: ex.w, f: flusher}
	// The convert package parses upstream chat SSE internally; tap the raw
	// stream here so the live view still sees per-delta token growth.
	if cb := ex.live.sseObserver(config.ProtoChat); cb != nil {
		body = &lineObserverReader{r: body, cb: cb}
	}
	result, err := convert.StreamChatToResponses(ex.req.Context(), sw, body, clientModel, namespaces)
	if err != nil {
		s.log.Warn("stream conversion failed", "err", err)
		ex.stat.Error = "stream conversion: " + err.Error()
	}
	if result != nil {
		if result.Usage != nil {
			ex.stat.Usage = toStatsUsage(result.Usage)
		}
		if !result.FirstTokenAt.IsZero() {
			ex.stat.TTFT = result.FirstTokenAt.Sub(ex.start)
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
	errBody, _ := readLimited(resp.Body, maxErrorBodySize)
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
