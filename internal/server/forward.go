package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/stats"
	"github.com/abowloflrf/apid/internal/trace"
	"github.com/abowloflrf/apid/internal/types"
)

// reqSniff reads only the top-level model / stream; both protocols share these
// field names. A parse failure yields zero values, which is harmless.
type reqSniff struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// forwardRaw handles a same-protocol route: forward the body verbatim, only
// rewriting model when effModel is non-empty. stat.ClientModel/Stream are
// already filled by handleRoute.
func (s *Server) forwardRaw(tg *target, effModel string, w http.ResponseWriter, r *http.Request, bodyBytes []byte, traceEntry *trace.Entry, stat *stats.Record, start time.Time) {
	fwdBody := bodyBytes
	if effModel != "" {
		b, err := overrideModel(bodyBytes, effModel)
		if err != nil {
			stat.Error = "override model: " + err.Error()
			writeError(w, http.StatusBadRequest, "failed to parse request body: "+err.Error())
			return
		}
		fwdBody = b
		if traceEntry != nil {
			if path := traceEntry.Dump("upstream", fwdBody); path != "" {
				log.Printf("trace upstream request -> %s", path)
			}
		}
	}

	resp, err := tg.client.Forward(r.Context(), fwdBody, r.Header.Get("Authorization"))
	if err != nil {
		stat.Error = "upstream: " + err.Error()
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	stat.UpstreamStatus = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		passUpstreamError(w, resp, stat)
		return
	}

	if stat.Stream {
		s.forwardStream(tg, w, resp, stat, start)
		return
	}
	forwardJSON(tg, w, resp, stat)
}

// forwardJSON forwards a non-stream response verbatim and parses usage by the
// upstream protocol.
func forwardJSON(tg *target, w http.ResponseWriter, resp *http.Response, stat *stats.Record) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		stat.Error = "read upstream response: " + err.Error()
		writeError(w, http.StatusBadGateway, "failed to read upstream response: "+err.Error())
		return
	}
	stat.Usage = extractUsage(tg.cfg.Protocol, body)

	copyContentType(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// forwardStream forwards an SSE response verbatim, tee-parsing usage and TTFT.
func (s *Server) forwardStream(tg *target, w http.ResponseWriter, resp *http.Response, stat *stats.Record, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		stat.Error = "streaming not supported"
		writeError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}
	copyContentType(w, resp)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	usage, firstAt, err := forwardSSE(&sseWriter{w: w, f: flusher}, resp.Body, tg.cfg.Protocol)
	if err != nil {
		log.Printf("sse forward error: %v", err)
		stat.Error = "sse forward: " + err.Error()
	}
	if usage != nil {
		stat.Usage = usage
	}
	if !firstAt.IsZero() {
		stat.TTFT = firstAt.Sub(start)
	}
}

// overrideModel rewrites the top-level model, keeping other fields as raw bytes
// to avoid number-precision loss and key reordering.
func overrideModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil { // e.g. body is "null": Unmarshal succeeds but leaves a nil map.
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = mb
	return json.Marshal(m)
}

// extractUsage 按输出协议从非流式响应体里抽取 usage。
func extractUsage(proto config.Protocol, body []byte) *stats.Usage {
	switch proto {
	case config.ProtoChat:
		var r struct {
			Usage *types.ChatUsage `json:"usage"`
		}
		if json.Unmarshal(body, &r) == nil {
			return toStatsUsage(r.Usage)
		}
	case config.ProtoResponses:
		var r struct {
			Usage *types.ResponseUsage `json:"usage"`
		}
		if json.Unmarshal(body, &r) == nil {
			return responseUsageToStats(r.Usage)
		}
	}
	return nil
}

// responseUsageToStats 把 Responses 协议的 usage 映射到 stats.Usage。
func responseUsageToStats(u *types.ResponseUsage) *stats.Usage {
	if u == nil {
		return nil
	}
	out := &stats.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.InputTokensDetails != nil {
		out.CachedTokens = u.InputTokensDetails.CachedTokens
	}
	return out
}

// copyContentType 把上游响应的 Content-Type 透传给客户端（纯转发保真）。
func copyContentType(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
}
