package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/protocol"
	"github.com/abowloflrf/apid/stats"
)

// reqSniff reads only the top-level model / stream; both protocols share these
// field names. A parse failure yields zero values, which is harmless.
type reqSniff struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// Limits for body reads. These guard against memory exhaustion from
// abnormally large or malicious payloads; normal LLM requests are well
// under these thresholds.
const (
	maxRequestBody   = 10 * 1024 * 1024 // 10 MB - client request body
	maxResponseBody  = 10 * 1024 * 1024 // 10 MB - upstream non-stream response body
	maxErrorBodySize = 1 * 1024 * 1024  // 1 MB  - upstream error response body
)

var errBodyTooLarge = errors.New("body exceeds size limit")

// readLimited reads at most max bytes from r. If the source contains more
// than max bytes, it returns the first max bytes (truncated) and
// errBodyTooLarge so callers can distinguish truncation from a normal EOF.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return body, err
	}
	if int64(len(body)) > max {
		return body[:max], errBodyTooLarge
	}
	return body, nil
}

// forwardRaw handles a same-protocol route: forward the body verbatim, only
// rewriting model when ex.model is non-empty. stat.ClientModel/Stream are
// already filled by handleRoute.
func (s *Server) forwardRaw(ex *exchange) {
	fwdBody := ex.body
	if ex.model != "" {
		b, err := overrideModel(ex.body, ex.model)
		if err != nil {
			ex.stat.Error = "override model: " + err.Error()
			writeError(ex.w, http.StatusBadRequest, "failed to parse request body: "+err.Error())
			return
		}
		fwdBody = b
		if ex.trace != nil {
			if path := ex.trace.Dump("upstream", fwdBody); path != "" {
				s.log.Info("trace upstream request", "file", path)
			}
		}
	}

	resp, err := ex.target.client.ForwardWithQuery(ex.req.Context(), fwdBody, ex.req.Header, ex.req.URL.RawQuery)
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

	if ex.stat.Stream {
		s.forwardStream(ex, resp)
		return
	}
	forwardJSON(ex, resp)
}

// forwardJSON forwards a non-stream response verbatim and parses usage by the
// upstream protocol.
func forwardJSON(ex *exchange, resp *http.Response) {
	body, err := readLimited(resp.Body, maxResponseBody)
	if err != nil {
		if err == errBodyTooLarge {
			ex.stat.Error = "upstream response exceeds size limit"
			writeError(ex.w, http.StatusBadGateway, "upstream response too large")
			return
		}
		ex.stat.Error = "read upstream response: " + err.Error()
		writeError(ex.w, http.StatusBadGateway, "failed to read upstream response: "+err.Error())
		return
	}
	ex.stat.Usage = extractUsage(ex.target.cfg.Protocol, body)

	copyContentType(ex.w, resp)
	ex.w.WriteHeader(resp.StatusCode)
	_, _ = ex.w.Write(body)
}

// forwardStream forwards an SSE response verbatim, tee-parsing usage and TTFT.
func (s *Server) forwardStream(ex *exchange, resp *http.Response) {
	flusher, ok := ex.w.(http.Flusher)
	if !ok {
		ex.stat.Error = "streaming not supported"
		writeError(ex.w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}
	copyContentType(ex.w, resp)
	ex.w.Header().Set("Cache-Control", "no-cache")
	ex.w.Header().Set("Connection", "keep-alive")
	ex.w.WriteHeader(resp.StatusCode)

	usage, firstAt, err := forwardSSE(ex.req.Context(), &sseWriter{w: ex.w, f: flusher}, resp.Body, ex.target.cfg.Protocol)
	if err != nil {
		switch {
		case errors.Is(err, errClientGone):
			// Stream already completed; client disconnected while draining the
			// tail. Benign - must not be recorded as an error (it would pollute
			// the error rate even though upstream succeeded).
			s.log.Debug("stream completed but client disconnected during drain",
				"model", ex.stat.UpstreamModel)
		case errors.Is(err, errClientAborted):
			// Client disconnected before the stream finished. Not an upstream
			// fault, but the request did not complete - record it with a
			// distinct message instead of the upstream "sse forward" prefix.
			s.log.Info("stream aborted by client before completion",
				"model", ex.stat.UpstreamModel)
			ex.stat.Error = "client aborted"
		default:
			s.log.Warn("sse forward failed", "err", err)
			ex.stat.Error = "sse forward: " + err.Error()
		}
	}
	if usage != nil {
		ex.stat.Usage = usage
	}
	if !firstAt.IsZero() {
		ex.stat.TTFT = firstAt.Sub(ex.start)
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
			Usage *protocol.ChatUsage `json:"usage"`
		}
		if json.Unmarshal(body, &r) == nil {
			return toStatsUsage(r.Usage)
		}
	case config.ProtoResponses:
		var r struct {
			Usage *protocol.ResponseUsage `json:"usage"`
		}
		if json.Unmarshal(body, &r) == nil {
			return responseUsageToStats(r.Usage)
		}
	case config.ProtoAnthropic:
		var r struct {
			Usage *protocol.AnthropicUsage `json:"usage"`
		}
		if json.Unmarshal(body, &r) == nil {
			return anthropicUsageToStats(r.Usage)
		}
	}
	return nil
}

// responseUsageToStats 把 Responses 协议的 usage 映射到 stats.Usage。
func responseUsageToStats(u *protocol.ResponseUsage) *stats.Usage {
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

// anthropicUsageToStats maps Anthropic's split cache fields into the common
// stats shape. InputTokens includes normal input, cache creation, and cache read
// tokens so TotalTokens remains the full request size.
func anthropicUsageToStats(u *protocol.AnthropicUsage) *stats.Usage {
	if u == nil {
		return nil
	}
	input := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return &stats.Usage{
		InputTokens:         input,
		OutputTokens:        u.OutputTokens,
		TotalTokens:         input + u.OutputTokens,
		CachedTokens:        u.CacheReadInputTokens,
		CacheCreationTokens: u.CacheCreationInputTokens,
	}
}

// copyContentType 把上游响应的 Content-Type 透传给客户端（纯转发保真）。
func copyContentType(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
}
