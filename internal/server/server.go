// Package server 提供对外的 HTTP 服务：按配置的多条路由分派，
// 同协议纯转发、异协议做协议转换，全程采集请求指标。
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/convert"
	"github.com/abowloflrf/apid/internal/stats"
	"github.com/abowloflrf/apid/internal/store"
	"github.com/abowloflrf/apid/internal/trace"
	"github.com/abowloflrf/apid/internal/types"
	"github.com/abowloflrf/apid/internal/upstream"
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
	routes    map[string]*route  // exposed path -> route
	upstreams map[string]*target // upstream name -> target
	tracer    *trace.Tracer
	recorder  *stats.Recorder
}

// New builds a Server. st may be nil (stats disabled). Each upstream gets one
// shared client; tracer/recorder are global.
func New(cfg config.Config, st *store.Store) *Server {
	upstreams := make(map[string]*target, len(cfg.Upstreams))
	for _, uc := range cfg.Upstreams {
		upstreams[uc.Name] = &target{
			cfg:    uc,
			client: upstream.New(uc.BaseURL, uc.Path, uc.APIKey),
		}
	}
	routes := make(map[string]*route, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		routes[rc.Path] = &route{cfg: rc}
	}
	return &Server{
		routes:    routes,
		upstreams: upstreams,
		tracer:    trace.New(cfg.TraceDir),
		recorder:  stats.NewRecorder(st, 0),
	}
}

// resolve picks the target and matched rule for a request's model on this
// route, returning nil target if no rule matches.
func (s *Server) resolve(rt *route, model string) (*target, config.ModelRule) {
	rule, ok := rt.cfg.Resolve(model)
	if !ok {
		return nil, config.ModelRule{}
	}
	return s.upstreams[rule.Upstream], rule
}

// effectiveModel returns the model to forward: the rule's override (incl. ""
// for explicit pass-through) wins over the upstream default; an empty result
// means pass the client's model through unchanged.
func effectiveModel(rule config.ModelRule, up config.Upstream) string {
	if rule.Model != nil {
		return *rule.Model
	}
	return up.Model
}

// Close 释放 Recorder 的后台 worker；底层 *sql.DB 由 store 关闭。
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.recorder.Close()
}

// Handler 返回配置好路由的 http.Handler。每条路由注册到其暴露路径，
// config 已保证路径唯一，ServeMux 不会冲突。
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
	return mux
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
		log.Printf("access method=%s path=%s model=%q stream=%t upstream_url=%q upstream_model=%q upstream_status=%d status=%d duration=%s",
			r.Method, r.URL.Path, stat.ClientModel, stat.Stream,
			stat.UpstreamURL, stat.UpstreamModel, stat.UpstreamStatus,
			rec.statusCode(), time.Since(start).Round(time.Millisecond))
	}()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		stat.Error = "read request body: " + err.Error()
		writeError(rec, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}

	// model is the routing key; both protocols carry it at the top level.
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
	stat.UpstreamModel = sniff.Model
	if effModel != "" {
		stat.UpstreamModel = effModel
	}

	traceEntry := s.tracer.Begin(r.Method, r.URL.RequestURI())
	if path := traceEntry.Dump(string(rt.cfg.InputProtocol), bodyBytes); path != "" {
		log.Printf("trace request -> %s", path)
	}

	// Same protocol on both ends => raw forward; otherwise convert.
	if rt.cfg.InputProtocol == tg.cfg.Protocol {
		s.forwardRaw(tg, effModel, rec, r, bodyBytes, traceEntry, &stat, start)
		return
	}
	s.convertResponsesToChat(tg, effModel, rec, r, bodyBytes, traceEntry, &stat, start)
}

// convertResponsesToChat handles a responses -> chat route: parse Responses ->
// convert to Chat -> forward -> stream/non-stream response conversion.
func (s *Server) convertResponsesToChat(tg *target, effModel string, w http.ResponseWriter, r *http.Request, bodyBytes []byte, traceEntry *trace.Entry, stat *stats.Record, start time.Time) {
	var req types.ResponsesRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		stat.Error = "parse request body: " + err.Error()
		writeError(w, http.StatusBadRequest, "failed to parse request body: "+err.Error())
		return
	}

	// namespaces splits MCP tool function_call names back from the upstream's
	// flat name into local name + namespace, so clients matching on
	// {name, namespace} (e.g. Codex) don't reject as unsupported.
	chatReq, namespaces, err := convert.ResponsesToChat(&req)
	if err != nil {
		stat.Error = "convert request: " + err.Error()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if effModel != "" {
		chatReq.Model = effModel
	}
	stat.UpstreamModel = chatReq.Model

	// 落盘转换后的 Chat 请求（即实际转发给上游的 body），与上面的请求配对。
	if traceEntry != nil {
		if chatBytes, err := json.Marshal(chatReq); err != nil {
			log.Printf("trace: failed to marshal chat request: %v", err)
		} else if path := traceEntry.Dump("chat", chatBytes); path != "" {
			log.Printf("trace chat request -> %s", path)
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

	// 上游返回非 2xx 时，打印错误日志并原样把错误体回传给客户端。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		passUpstreamError(w, resp, stat)
		return
	}

	if req.Stream {
		s.streamResponse(w, resp.Body, req.Model, namespaces, stat, start)
		return
	}
	s.jsonResponse(w, resp.Body, namespaces, stat)
}

// jsonResponse 处理非流式：解析上游 Chat 响应并转成 Responses 响应，并把
// usage 填入 stat（成功路径由 defer 统一落盘）。
func (s *Server) jsonResponse(w http.ResponseWriter, body io.Reader, namespaces map[string]convert.NamespacedTool, stat *stats.Record) {
	var chatResp types.ChatResponse
	if err := json.NewDecoder(body).Decode(&chatResp); err != nil {
		stat.Error = "parse upstream response: " + err.Error()
		writeError(w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}
	stat.Usage = toStatsUsage(chatResp.Usage)

	out := convert.ChatToResponses(&chatResp, namespaces)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("failed to write response: %v", err)
	}
}

// streamResponse 处理流式：把上游 SSE 转换为 Responses 事件流。
// 流末的 usage、首 token 时刻通过 result 带回到 stat，由 defer 落盘。
// start 是整条请求的起点，用于把首 token 时刻换算成 TTFT(客户端视角的端到端首字节)。
func (s *Server) streamResponse(w http.ResponseWriter, body io.Reader, model string, namespaces map[string]convert.NamespacedTool, stat *stats.Record, start time.Time) {
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
	result, err := convert.StreamChatToResponses(sw, body, model, namespaces)
	if err != nil {
		log.Printf("stream conversion error: %v", err)
		stat.Error = "stream conversion: " + err.Error()
	}
	if result != nil {
		if result.Usage != nil {
			stat.Usage = toStatsUsage(result.Usage)
		}
		if !result.FirstTokenAt.IsZero() {
			stat.TTFT = result.FirstTokenAt.Sub(start)
		}
	}
}

// passUpstreamError forwards an upstream non-2xx error body verbatim and records
// it. The upstream Content-Type is preserved (so pure-forward stays byte-faithful),
// falling back to JSON only when the upstream sent none.
func passUpstreamError(w http.ResponseWriter, resp *http.Response, stat *stats.Record) {
	errBody, _ := io.ReadAll(resp.Body)
	log.Printf("upstream error status=%d body=%s", resp.StatusCode, errBody)
	stat.Error = truncate(string(errBody), 512)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
}

// toStatsUsage 把上游 ChatUsage 映射到 stats.Usage。nil 入参返回 nil，
// 调用方在失败时无需特殊处理，stat.Usage 保持 nil，stats 层会写 0。
func toStatsUsage(u *types.ChatUsage) *stats.Usage {
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

// truncate 截断超长字符串，避免上游错误体把 SQLite 撑爆。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// respRecorder 包装 http.ResponseWriter，记录最终写出的状态码用于 access log。
// 同时透传 Flush，以兼容流式输出。
type respRecorder struct {
	http.ResponseWriter
	status int
}

func (r *respRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// statusCode 返回实际状态码；未显式调用 WriteHeader 时按 200 处理。
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

// sseWriter 把 http.ResponseWriter + Flusher 适配为 convert.SSEWriter。
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
