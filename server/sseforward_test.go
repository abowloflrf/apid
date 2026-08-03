package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/stats"
)

// ---------- parse helpers ----------

// TestParseResponsesSSE covers the Responses event stream parser: a *.delta event
// stamps TTFT, response.completed carries usage and marks completion, and
// malformed/irrelevant events are no-ops.
func TestParseResponsesSSE(t *testing.T) {
	t.Run("delta sets first-token time, not completed", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		var completed bool
		parseResponsesSSE(`{"type":"response.output_text.delta","delta":"hi"}`, &usage, &first, &completed)
		if first.IsZero() {
			t.Error("first-token time not set on .delta event")
		}
		if usage != nil {
			t.Errorf("usage = %+v, want nil on delta event", usage)
		}
		if completed {
			t.Error("delta event should not mark completed")
		}
	})

	t.Run("first delta wins", func(t *testing.T) {
		var usage *stats.Usage
		first := time.Unix(1, 0)
		var completed bool
		parseResponsesSSE(`{"type":"response.output_text.delta","delta":"x"}`, &usage, &first, &completed)
		if !first.Equal(time.Unix(1, 0)) {
			t.Error("already-set first-token time was overwritten")
		}
	})

	t.Run("completed carries usage and marks completion", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		var completed bool
		parseResponsesSSE(`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":3}}}}`, &usage, &first, &completed)
		if usage == nil {
			t.Fatal("usage not parsed from response.completed")
		}
		if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 || usage.CachedTokens != 3 {
			t.Errorf("usage = %+v, want {10 5 15 3}", usage)
		}
		if !first.IsZero() {
			t.Error("non-delta event should not set first-token time")
		}
		if !completed {
			t.Error("response.completed should mark completed")
		}
	})

	t.Run("malformed json is a no-op", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		var completed bool
		parseResponsesSSE(`{not json`, &usage, &first, &completed)
		if usage != nil || !first.IsZero() || completed {
			t.Errorf("malformed data mutated state: usage=%+v first=%v completed=%v", usage, first, completed)
		}
	})
}

func TestParseChatSSE(t *testing.T) {
	t.Run("content delta sets first-token, not completed", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		var completed bool
		parseChatSSE(`{"choices":[{"delta":{"content":"hi"}}]}`, &usage, &first, &completed)
		if first.IsZero() {
			t.Error("first-token not set on content delta")
		}
		if completed {
			t.Error("delta chunk should not mark completed")
		}
	})

	t.Run("finish_reason marks completed", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		var completed bool
		parseChatSSE(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, &usage, &first, &completed)
		if !completed {
			t.Error("finish_reason should mark completed")
		}
	})

	t.Run("usage marks completed", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		var completed bool
		parseChatSSE(`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, &usage, &first, &completed)
		if usage == nil || usage.TotalTokens != 5 {
			t.Errorf("usage not parsed: %+v", usage)
		}
		if !completed {
			t.Error("usage chunk should mark completed")
		}
	})
}

func TestParseAnthropicSSEUsage(t *testing.T) {
	var usage *stats.Usage
	var first time.Time
	var completed bool

	// message_start carries input usage but must NOT mark completion.
	parseAnthropicSSE(`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":1,"cache_read_input_tokens":2,"output_tokens":0}}}`, &usage, &first, &completed)
	if usage == nil {
		t.Fatal("usage not parsed from message_start")
	}
	if completed {
		t.Fatal("message_start must not mark completed")
	}

	parseAnthropicSSE(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, &usage, &first, &completed)
	if first.IsZero() {
		t.Error("content_block_delta should set first-token time")
	}
	if completed {
		t.Error("content_block_delta must not mark completed")
	}

	parseAnthropicSSE(`{"type":"message_delta","usage":{"output_tokens":3}}`, &usage, &first, &completed)
	if usage.InputTokens != 8 || usage.OutputTokens != 3 || usage.TotalTokens != 11 ||
		usage.CachedTokens != 2 || usage.CacheCreationTokens != 1 {
		t.Errorf("usage = %+v, want input=8 output=3 total=11 cache_read=2 cache_creation=1", usage)
	}
	if completed {
		t.Error("message_delta must not mark completed")
	}

	parseAnthropicSSE(`{"type":"message_stop"}`, &usage, &first, &completed)
	if !completed {
		t.Error("message_stop should mark completed")
	}
}

// ---------- forwardSSE client-disconnect classification ----------
//
// okSink / failSink / errReader are defined in forward_test.go and reused here.
// These tests cover the new classification: a client disconnect after the stream
// completed is benign (errClientGone), while one before completion is an abort
// (errClientAborted). A genuine upstream read error stays a "read upstream" error.

// blockingReader serves preset data, then blocks. If ctx is set, a blocked Read
// returns ctx.Err() when ctx is cancelled (mimicking an http.ResponseBody that
// observes the request context). onDrained fires once when the preset data is
// exhausted.
type blockingReader struct {
	data      []byte
	pos       int
	ctx       context.Context
	onDrained func()
	drained   bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		if !r.drained && r.pos >= len(r.data) {
			r.drained = true
			if r.onDrained != nil {
				r.onDrained()
			}
		}
		return n, nil
	}
	if r.ctx != nil {
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	select {} // no ctx: block forever (misconfigured test)
}

const (
	completedResponsesChunk = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	deltaResponsesChunk     = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
)

func TestForwardSSE_CompletedClientGone(t *testing.T) {
	// Stream completes, then ctx is cancelled at the next loop-top check (point 1).
	ctx, cancel := context.WithCancel(context.Background())
	src := &blockingReader{data: []byte(completedResponsesChunk)}
	src.onDrained = func() { cancel() }
	_, _, err := forwardSSE(ctx, okSink{}, src, config.ProtoResponses, nil)
	if !errors.Is(err, errClientGone) {
		t.Errorf("want errClientGone, got %v", err)
	}
}

func TestForwardSSE_AbortedClientGone(t *testing.T) {
	// Stream NOT completed (only a delta), client cancels -> errClientAborted.
	ctx, cancel := context.WithCancel(context.Background())
	src := &blockingReader{data: []byte(deltaResponsesChunk)}
	src.onDrained = func() { cancel() }
	_, _, err := forwardSSE(ctx, okSink{}, src, config.ProtoResponses, nil)
	if !errors.Is(err, errClientAborted) {
		t.Errorf("want errClientAborted, got %v", err)
	}
}

func TestForwardSSE_ClientGoneDuringBlockedRead(t *testing.T) {
	// Stream completes, then a blocked Read is interrupted by ctx cancel (point 3).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &blockingReader{data: []byte(completedResponsesChunk), ctx: ctx}
	go func() {
		time.Sleep(10 * time.Millisecond) // let forwardSSE parse and enter the blocked Read
		cancel()
	}()
	_, _, err := forwardSSE(ctx, okSink{}, src, config.ProtoResponses, nil)
	if !errors.Is(err, errClientGone) {
		t.Errorf("want errClientGone, got %v", err)
	}
}

func TestForwardSSE_WriteFailureClassified(t *testing.T) {
	// Write fails before the line is parsed, so completed is still false ->
	// errClientAborted (not a benign drain).
	_, _, err := forwardSSE(context.Background(), failSink{}, strings.NewReader(deltaResponsesChunk), config.ProtoResponses, nil)
	if !errors.Is(err, errClientAborted) {
		t.Errorf("want errClientAborted, got %v", err)
	}
}

func TestForwardSSE_UpstreamReadErrorNotClientGone(t *testing.T) {
	// A non-EOF, non-cancel read error must surface as "read upstream", not as a
	// client-gone sentinel. errReader (from forward_test.go) yields one chunk
	// then a "conn reset" error.
	_, _, err := forwardSSE(context.Background(), okSink{}, &errReader{}, config.ProtoChat, nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if errors.Is(err, errClientGone) || errors.Is(err, errClientAborted) {
		t.Errorf("upstream read error misclassified as client disconnect: %v", err)
	}
	if !strings.Contains(err.Error(), "read upstream") {
		t.Errorf("error should mention read upstream, got: %v", err)
	}
}

func TestForwardSSERejectsOversizedLine(t *testing.T) {
	line := "data: " + strings.Repeat("x", maxSSELineSize) + "\n"
	_, _, err := forwardSSE(context.Background(), okSink{}, strings.NewReader(line), config.ProtoResponses, nil)
	if !errors.Is(err, errSSELineTooLarge) {
		t.Fatalf("error = %v, want errSSELineTooLarge", err)
	}
}

func TestIdleTimeoutReader(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	idle := &idleTimeoutReader{ctx: context.Background(), src: reader, timeout: 10 * time.Millisecond}
	_, err := idle.Read(make([]byte, 16))
	if !errors.Is(err, errSSEIdleTimeout) {
		t.Fatalf("error = %v, want errSSEIdleTimeout", err)
	}
}
