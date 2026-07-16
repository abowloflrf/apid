package server

import (
	"testing"
	"time"

	"github.com/abowloflrf/apid/stats"
)

// TestParseResponsesSSE covers the Responses event stream parser: a *.delta event
// stamps TTFT, response.completed carries usage, and malformed/irrelevant events
// are no-ops.
func TestParseResponsesSSE(t *testing.T) {
	t.Run("delta sets first-token time", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		parseResponsesSSE(`{"type":"response.output_text.delta","delta":"hi"}`, &usage, &first)
		if first.IsZero() {
			t.Error("first-token time not set on .delta event")
		}
		if usage != nil {
			t.Errorf("usage = %+v, want nil on delta event", usage)
		}
	})

	t.Run("first delta wins", func(t *testing.T) {
		var usage *stats.Usage
		first := time.Unix(1, 0)
		parseResponsesSSE(`{"type":"response.output_text.delta","delta":"x"}`, &usage, &first)
		if !first.Equal(time.Unix(1, 0)) {
			t.Error("already-set first-token time was overwritten")
		}
	})

	t.Run("completed carries usage", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		parseResponsesSSE(`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":3}}}}`, &usage, &first)
		if usage == nil {
			t.Fatal("usage not parsed from response.completed")
		}
		if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 || usage.CachedTokens != 3 {
			t.Errorf("usage = %+v, want {10 5 15 3}", usage)
		}
		if !first.IsZero() {
			t.Error("non-delta event should not set first-token time")
		}
	})

	t.Run("malformed json is a no-op", func(t *testing.T) {
		var usage *stats.Usage
		var first time.Time
		parseResponsesSSE(`{not json`, &usage, &first)
		if usage != nil || !first.IsZero() {
			t.Errorf("malformed data mutated state: usage=%+v first=%v", usage, first)
		}
	})
}

func TestParseAnthropicSSEUsage(t *testing.T) {
	var usage *stats.Usage
	var first time.Time

	parseAnthropicSSE(`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":1,"cache_read_input_tokens":2,"output_tokens":0}}}`, &usage, &first)
	parseAnthropicSSE(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, &usage, &first)
	parseAnthropicSSE(`{"type":"message_delta","usage":{"output_tokens":3}}`, &usage, &first)

	if usage == nil {
		t.Fatal("usage not parsed from Anthropic stream")
	}
	if usage.InputTokens != 8 || usage.OutputTokens != 3 || usage.TotalTokens != 11 ||
		usage.CachedTokens != 2 || usage.CacheCreationTokens != 1 {
		t.Errorf("usage = %+v, want input=8 output=3 total=11 cache_read=2 cache_creation=1", usage)
	}
	if first.IsZero() {
		t.Error("content_block_delta should set first-token time")
	}
}
