package server

// Live view of in-flight requests. Every proxied request registers itself in
// an in-memory registry for its lifetime; GET /stats/active snapshots it for
// the dashboard's live view, which polls and diffs snapshots client-side to
// show per-connection tokens/s.
//
// Output tokens are counted in real time while a stream is in flight: exact
// numbers are taken from usage whenever the protocol delivers them mid-stream
// (Anthropic message_start / message_delta, final Chat/Responses usage), and
// until then they are estimated from the delta text (CJK ~1 token per rune,
// other text ~4 chars per token). The `*_est` flags tell the UI which one it
// is looking at. Nothing here touches storage; the registry works with the
// stats DB disabled.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/protocol"
)

type liveRegistry struct {
	mu     sync.Mutex
	nextID uint64
	reqs   map[uint64]*liveRequest
}

func newLiveRegistry() *liveRegistry {
	return &liveRegistry{reqs: make(map[uint64]*liveRequest)}
}

// liveBegin carries the request facts known at dispatch time.
type liveBegin struct {
	clientModel   string
	upstreamModel string
	clientProto   string
	upstreamProto string
	upstream      string
	path          string
	ua            string
	stream        bool
	inputEstimate int // rough token estimate from the request body size
}

type liveRequest struct {
	reg *liveRegistry
	id  uint64

	// Immutable after begin.
	start time.Time
	info  liveBegin

	mu           sync.Mutex
	outCJK       int // estimate basis: CJK runes seen in output deltas
	outOther     int // estimate basis: non-CJK runes seen in output deltas
	exactIn      int
	exactInSeen  bool
	exactOut     int
	exactOutSeen bool
	firstTokenAt time.Time
}

// begin registers a request and returns its live entry. Callers must pair it
// with end(). Nil-safe on the registry for tests that build a bare Server.
func (r *liveRegistry) begin(info liveBegin) *liveRequest {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	lr := &liveRequest{reg: r, id: r.nextID, start: time.Now(), info: info}
	r.reqs[lr.id] = lr
	return lr
}

func (lr *liveRequest) end() {
	if lr == nil {
		return
	}
	lr.reg.mu.Lock()
	delete(lr.reg.reqs, lr.id)
	lr.reg.mu.Unlock()
}

// sseObserver returns a per-line callback that keeps this entry's token
// counters current while an SSE stream is forwarded. Returns nil (no-op for
// forwardSSE) when the entry itself is nil.
func (lr *liveRequest) sseObserver(proto config.Protocol) func([]byte) {
	if lr == nil {
		return nil
	}
	return func(line []byte) { lr.observeLine(proto, line) }
}

func (lr *liveRequest) observeLine(proto config.Protocol, line []byte) {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	switch proto {
	case config.ProtoChat:
		lr.observeChat(data)
	case config.ProtoResponses:
		lr.observeResponses(data)
	case config.ProtoAnthropic:
		lr.observeAnthropic(data)
	}
}

func (lr *liveRequest) observeChat(data string) {
	var chunk protocol.ChatStreamChunk
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return
	}
	var text strings.Builder
	for _, ch := range chunk.Choices {
		text.WriteString(ch.Delta.Content)
		text.WriteString(ch.Delta.ReasoningContent)
		for _, tc := range ch.Delta.ToolCalls {
			text.WriteString(tc.Function.Arguments)
		}
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.addEstimateLocked(text.String())
	if u := chunk.Usage; u != nil {
		lr.setExactLocked(u.PromptTokens, true, u.CompletionTokens, true)
	}
}

// responsesLiveEvent is the minimal Responses SSE shape the live counter needs:
// the string delta of *.delta events plus the final response.usage.
type responsesLiveEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Response struct {
		Usage *protocol.ResponseUsage `json:"usage"`
	} `json:"response"`
}

func (lr *liveRequest) observeResponses(data string) {
	var ev responsesLiveEvent
	if json.Unmarshal([]byte(data), &ev) != nil {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if strings.HasSuffix(ev.Type, ".delta") {
		lr.addEstimateLocked(ev.Delta)
	}
	if u := ev.Response.Usage; u != nil {
		lr.setExactLocked(u.InputTokens, true, u.OutputTokens, true)
	}
}

func (lr *liveRequest) observeAnthropic(data string) {
	var ev protocol.AnthropicStreamEvent
	if json.Unmarshal([]byte(data), &ev) != nil {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.addEstimateLocked(ev.Delta.Text + ev.Delta.PartialJSON)
	// message_start carries input usage; message_delta carries a cumulative
	// output_tokens, so mid-stream counts are exact on this protocol.
	for _, u := range []*protocol.AnthropicUsage{ev.Message.Usage, ev.Usage} {
		if u == nil {
			continue
		}
		in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		lr.setExactLocked(in, in > 0, u.OutputTokens, u.OutputTokens > 0)
	}
}

// addEstimateLocked grows the estimate basis and stamps the first-token time.
func (lr *liveRequest) addEstimateLocked(text string) {
	if text == "" {
		return
	}
	if lr.firstTokenAt.IsZero() {
		lr.firstTokenAt = time.Now()
	}
	for _, r := range text {
		if r >= 0x2E80 { // CJK and beyond: ~1 token per rune
			lr.outCJK++
		} else {
			lr.outOther++
		}
	}
}

func (lr *liveRequest) setExactLocked(in int, inOK bool, out int, outOK bool) {
	if inOK {
		lr.exactIn, lr.exactInSeen = in, true
	}
	if outOK {
		lr.exactOut, lr.exactOutSeen = out, true
	}
}

// ---- snapshot API ----

type liveSnapshot struct {
	Now      int64          `json:"now"` // unix ms, for client-side clock skew correction
	Count    int            `json:"count"`
	Requests []liveReqEntry `json:"requests"`
}

type liveReqEntry struct {
	ID               uint64 `json:"id"`
	Start            int64  `json:"start"` // unix ms
	ElapsedMs        int64  `json:"elapsed_ms"`
	ClientModel      string `json:"client_model"`
	UpstreamModel    string `json:"upstream_model"`
	ClientProtocol   string `json:"client_protocol"`
	UpstreamProtocol string `json:"upstream_protocol"`
	Upstream         string `json:"upstream"`
	Mode             string `json:"mode"` // forward | convert
	Path             string `json:"path"`
	ClientUA         string `json:"client_ua"`
	Stream           bool   `json:"stream"`
	InputTokens      int    `json:"input_tokens"`
	InputEst         bool   `json:"input_est"`
	OutputTokens     int    `json:"output_tokens"`
	OutputEst        bool   `json:"output_est"`
	TTFTMs           *int64 `json:"ttft_ms"` // null until the first output token
}

func (lr *liveRequest) snapshot(now time.Time) liveReqEntry {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	e := liveReqEntry{
		ID:               lr.id,
		Start:            lr.start.UnixMilli(),
		ElapsedMs:        now.Sub(lr.start).Milliseconds(),
		ClientModel:      lr.info.clientModel,
		UpstreamModel:    lr.info.upstreamModel,
		ClientProtocol:   lr.info.clientProto,
		UpstreamProtocol: lr.info.upstreamProto,
		Upstream:         lr.info.upstream,
		Mode:             "forward",
		Path:             lr.info.path,
		ClientUA:         lr.info.ua,
		Stream:           lr.info.stream,
	}
	if lr.info.clientProto != lr.info.upstreamProto {
		e.Mode = "convert"
	}
	e.InputTokens, e.InputEst = lr.info.inputEstimate, true
	if lr.exactInSeen {
		e.InputTokens, e.InputEst = lr.exactIn, false
	}
	e.OutputTokens, e.OutputEst = lr.outCJK+(lr.outOther+3)/4, true
	if lr.exactOutSeen {
		e.OutputTokens, e.OutputEst = lr.exactOut, false
	}
	if !lr.firstTokenAt.IsZero() {
		ttft := lr.firstTokenAt.Sub(lr.start).Milliseconds()
		e.TTFTMs = &ttft
	}
	return e
}

func (r *liveRegistry) snapshot() liveSnapshot {
	now := time.Now()
	r.mu.Lock()
	reqs := make([]*liveRequest, 0, len(r.reqs))
	for _, lr := range r.reqs {
		reqs = append(reqs, lr)
	}
	r.mu.Unlock()

	out := liveSnapshot{Now: now.UnixMilli(), Count: len(reqs), Requests: make([]liveReqEntry, 0, len(reqs))}
	for _, lr := range reqs {
		out.Requests = append(out.Requests, lr.snapshot(now))
	}
	sort.Slice(out.Requests, func(i, j int) bool { return out.Requests[i].ID < out.Requests[j].ID })
	return out
}

// handleStatsActive serves the live snapshot. Unlike the other stats endpoints
// it does not need storage: the registry is purely in-memory.
func (s *Server) handleStatsActive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, s.live.snapshot())
}

// ---- line tee for the convert path ----

// lineObserverReader tees complete lines to cb as they pass through. The
// convert path parses upstream chat SSE inside the convert package, so the
// live counter taps the raw stream here instead.
type lineObserverReader struct {
	r   io.Reader
	cb  func([]byte)
	buf []byte
}

func (t *lineObserverReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.cb != nil {
		t.buf = append(t.buf, p[:n]...)
		for {
			i := bytes.IndexByte(t.buf, '\n')
			if i < 0 {
				break
			}
			t.cb(t.buf[:i+1])
			t.buf = t.buf[i+1:]
		}
	}
	return n, err
}
