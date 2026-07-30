package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
)

func TestLiveRegistryBeginEndSnapshot(t *testing.T) {
	reg := newLiveRegistry()
	lr := reg.begin(liveBegin{
		clientModel: "gpt-5", upstreamModel: "deepseek-v3",
		clientProto: string(config.ProtoResponses), upstreamProto: string(config.ProtoChat),
		upstream: "deepseek", path: "/v1/responses", stream: true, inputEstimate: 42,
	})

	snap := reg.snapshot()
	if snap.Count != 1 || len(snap.Requests) != 1 {
		t.Fatalf("count = %d, want 1", snap.Count)
	}
	e := snap.Requests[0]
	if e.ClientModel != "gpt-5" || e.UpstreamModel != "deepseek-v3" {
		t.Errorf("models = %q -> %q", e.ClientModel, e.UpstreamModel)
	}
	if e.Mode != "convert" {
		t.Errorf("mode = %q, want convert", e.Mode)
	}
	if e.InputTokens != 42 || !e.InputEst {
		t.Errorf("input = %d est=%v, want 42 est", e.InputTokens, e.InputEst)
	}
	if e.OutputTokens != 0 || !e.OutputEst {
		t.Errorf("output = %d est=%v, want 0 est", e.OutputTokens, e.OutputEst)
	}
	if e.TTFTMs != nil {
		t.Errorf("ttft = %v, want nil before first token", *e.TTFTMs)
	}

	lr.end()
	if got := reg.snapshot().Count; got != 0 {
		t.Fatalf("count after end = %d, want 0", got)
	}
}

func TestLiveObserveChatEstimateThenExact(t *testing.T) {
	reg := newLiveRegistry()
	lr := reg.begin(liveBegin{clientProto: "x", upstreamProto: "x"})
	cb := lr.sseObserver(config.ProtoChat)

	// 8 ASCII chars => ~2 estimated tokens; first token time stamped.
	cb([]byte(`data: {"choices":[{"delta":{"content":"abcdefgh"}}]}` + "\n"))
	e := reg.snapshot().Requests[0]
	if e.OutputTokens != 2 || !e.OutputEst {
		t.Errorf("estimated output = %d est=%v, want 2 est", e.OutputTokens, e.OutputEst)
	}
	if e.TTFTMs == nil {
		t.Error("ttft not stamped after first delta")
	}

	// CJK counts ~1 token per rune.
	cb([]byte(`data: {"choices":[{"delta":{"reasoning_content":"你好"}}]}` + "\n"))
	if e = reg.snapshot().Requests[0]; e.OutputTokens != 4 {
		t.Errorf("output after CJK = %d, want 4", e.OutputTokens)
	}

	// Final usage overrides the estimate on both sides.
	cb([]byte(`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7}}` + "\n"))
	e = reg.snapshot().Requests[0]
	if e.OutputTokens != 7 || e.OutputEst {
		t.Errorf("exact output = %d est=%v, want 7 exact", e.OutputTokens, e.OutputEst)
	}
	if e.InputTokens != 100 || e.InputEst {
		t.Errorf("exact input = %d est=%v, want 100 exact", e.InputTokens, e.InputEst)
	}
}

func TestLiveObserveAnthropicUsage(t *testing.T) {
	reg := newLiveRegistry()
	lr := reg.begin(liveBegin{})
	cb := lr.sseObserver(config.ProtoAnthropic)

	// message_start: exact input right away.
	cb([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"cache_read_input_tokens":10}}}` + "\n"))
	e := reg.snapshot().Requests[0]
	if e.InputTokens != 60 || e.InputEst {
		t.Errorf("input = %d est=%v, want 60 exact", e.InputTokens, e.InputEst)
	}

	// Text and tool-args deltas grow the estimate.
	cb([]byte(`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello world!"}}` + "\n"))
	cb([]byte(`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}` + "\n"))
	if e = reg.snapshot().Requests[0]; e.OutputTokens == 0 || !e.OutputEst {
		t.Errorf("output = %d est=%v, want >0 est", e.OutputTokens, e.OutputEst)
	}

	// message_delta: cumulative exact output mid-stream.
	cb([]byte(`data: {"type":"message_delta","usage":{"output_tokens":33}}` + "\n"))
	e = reg.snapshot().Requests[0]
	if e.OutputTokens != 33 || e.OutputEst {
		t.Errorf("output = %d est=%v, want 33 exact", e.OutputTokens, e.OutputEst)
	}
}

func TestLiveObserveResponses(t *testing.T) {
	reg := newLiveRegistry()
	lr := reg.begin(liveBegin{})
	cb := lr.sseObserver(config.ProtoResponses)

	cb([]byte(`data: {"type":"response.output_text.delta","delta":"abcd"}` + "\n"))
	if e := reg.snapshot().Requests[0]; e.OutputTokens != 1 || !e.OutputEst {
		t.Errorf("output = %d est=%v, want 1 est", e.OutputTokens, e.OutputEst)
	}

	cb([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":22}}}` + "\n"))
	e := reg.snapshot().Requests[0]
	if e.InputTokens != 11 || e.OutputTokens != 22 || e.InputEst || e.OutputEst {
		t.Errorf("usage = in %d/%v out %d/%v, want 11/22 exact", e.InputTokens, e.InputEst, e.OutputTokens, e.OutputEst)
	}
}

func TestLiveNilSafety(t *testing.T) {
	var lr *liveRequest
	if lr.sseObserver(config.ProtoChat) != nil {
		t.Error("nil liveRequest should yield nil observer")
	}
	lr.end() // must not panic
	var reg *liveRegistry
	if reg.begin(liveBegin{}) != nil {
		t.Error("nil registry begin should return nil")
	}
}

func TestLineObserverReaderSplitsLines(t *testing.T) {
	var lines []string
	r := &lineObserverReader{
		r:  strings.NewReader("data: aa\ndata: bb\n\ndata: cc"),
		cb: func(b []byte) { lines = append(lines, string(b)) },
	}
	buf := make([]byte, 5) // force line splits across reads
	for {
		if _, err := r.Read(buf); err != nil {
			break
		}
	}
	want := []string{"data: aa\n", "data: bb\n", "\n"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestHandleStatsActive(t *testing.T) {
	srv := New(config.Config{}, nil, nil)
	lr := srv.live.begin(liveBegin{clientModel: "m1", clientProto: "p", upstreamProto: "p"})
	defer lr.end()

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats/active", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var snap liveSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Count != 1 || snap.Requests[0].ClientModel != "m1" {
		t.Errorf("snapshot = %+v", snap)
	}
	if snap.Now == 0 {
		t.Error("now not set")
	}
}
