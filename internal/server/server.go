// Package server 提供对外的 Responses API HTTP 服务。
package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/convert"
	"github.com/abowloflrf/apid/internal/trace"
	"github.com/abowloflrf/apid/internal/types"
	"github.com/abowloflrf/apid/internal/upstream"
)

type Server struct {
	cfg      config.Config
	upstream *upstream.Client
	tracer   *trace.Tracer
}

func New(cfg config.Config) *Server {
	return &Server{
		cfg:      cfg,
		upstream: upstream.New(cfg.UpstreamBaseURL, cfg.UpstreamAPIKey),
		tracer:   trace.New(cfg.TraceDir),
	}
}

// Handler 返回配置好路由的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// handleResponses 处理 Responses API 请求：
// 解析 -> 转换为 Chat 请求 -> 转发上游 -> 转换响应回 Responses 形式。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &respRecorder{ResponseWriter: w}

	var req types.ResponsesRequest
	upstreamStatus := 0
	// 单条 access log：无论成功或失败都在请求结束时打印一行。
	defer func() {
		log.Printf("access method=%s path=%s model=%q stream=%t tools=%d upstream=%d status=%d duration=%s",
			r.Method, r.URL.Path, req.Model, req.Stream, len(req.Tools),
			upstreamStatus, rec.statusCode(), time.Since(start).Round(time.Millisecond))
	}()

	// 读取原始请求体：既用于 TRACE 落盘，也用于后续解析。
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	// 同一请求落盘多份：先存原始 Responses 请求，转换后再存对应的 Chat 请求。
	traceEntry := s.tracer.Begin(r.Method, r.URL.RequestURI())
	if path := traceEntry.Dump("responses", bodyBytes); path != "" {
		log.Printf("trace request -> %s", path)
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(rec, http.StatusBadRequest, "failed to parse request body: "+err.Error())
		return
	}

	// namespaces：把响应里 MCP 工具的 function_call 从上游扁平名拆回本地名 +
	// namespace，否则 Codex 等客户端按 {name, namespace} 精确匹配会失败(unsupported call)。
	chatReq, namespaces, err := convert.ResponsesToChat(&req)
	if err != nil {
		writeError(rec, http.StatusBadRequest, err.Error())
		return
	}

	// 配置了上游实际模型名时，覆盖客户端请求里的 model。
	if s.cfg.UpstreamModel != "" {
		chatReq.Model = s.cfg.UpstreamModel
	}

	// 落盘转换后的 Chat 请求（即实际转发给上游的 body），与上面的 responses 配对。
	if traceEntry != nil {
		if chatBytes, err := json.Marshal(chatReq); err != nil {
			log.Printf("trace: failed to marshal chat request: %v", err)
		} else if path := traceEntry.Dump("chat", chatBytes); path != "" {
			log.Printf("trace chat request -> %s", path)
		}
	}

	resp, err := s.upstream.ChatCompletions(r.Context(), chatReq, r.Header.Get("Authorization"))
	if err != nil {
		writeError(rec, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	upstreamStatus = resp.StatusCode

	// 上游返回非 2xx 时，打印错误日志并原样把错误体回传给客户端。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		log.Printf("upstream error status=%d body=%s", resp.StatusCode, errBody)
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(resp.StatusCode)
		_, _ = rec.Write(errBody)
		return
	}

	if req.Stream {
		s.streamResponse(rec, resp.Body, req.Model, namespaces)
		return
	}
	s.jsonResponse(rec, resp.Body, namespaces)
}

// jsonResponse 处理非流式：解析上游 Chat 响应并转成 Responses 响应。
func (s *Server) jsonResponse(w http.ResponseWriter, body io.Reader, namespaces map[string]convert.NamespacedTool) {
	var chatResp types.ChatResponse
	if err := json.NewDecoder(body).Decode(&chatResp); err != nil {
		writeError(w, http.StatusBadGateway, "failed to parse upstream response: "+err.Error())
		return
	}

	out := convert.ChatToResponses(&chatResp, namespaces)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("failed to write response: %v", err)
	}
}

// streamResponse 处理流式：把上游 SSE 转换为 Responses 事件流。
func (s *Server) streamResponse(w http.ResponseWriter, body io.Reader, model string, namespaces map[string]convert.NamespacedTool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sw := &sseWriter{w: w, f: flusher}
	if err := convert.StreamChatToResponses(sw, body, model, namespaces); err != nil {
		log.Printf("stream conversion error: %v", err)
	}
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
