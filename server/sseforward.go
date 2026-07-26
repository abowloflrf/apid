package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/protocol"
	"github.com/abowloflrf/apid/stats"
)

// forwardSSE streams upstream SSE to the client verbatim, tee-parsing usage and
// the first-token time. Parsing failures are swallowed (never affect forwarding),
// but a non-EOF read error or a client write error is returned so stats can flag
// a truncated stream. Returns end-of-stream usage and first content-delta time.
// Sentinel errors for client-side disconnects, classified by whether the
// stream had already reached its protocol completion signal.
var (
	// errClientGone: stream completed, client disconnected during drain. Benign,
	// not recorded as an error.
	errClientGone = errors.New("client gone")
	// errClientAborted: stream aborted by client before completion. Recorded as
	// an error, but the message distinguishes it from an upstream fault.
	errClientAborted = errors.New("client aborted")
)

func forwardSSE(ctx context.Context, dst sseSink, src io.Reader, proto config.Protocol) (*stats.Usage, time.Time, error) {
	reader := bufio.NewReader(src)
	var usage *stats.Usage
	var firstTokenAt time.Time
	var completed bool

	for {
		// Client gone (disconnect / shutdown): stop reading upstream.
		if err := ctx.Err(); err != nil {
			return usage, firstTokenAt, clientGoneErr(completed)
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := dst.Write(line); werr != nil {
				// Client gone: not an upstream fault.
				return usage, firstTokenAt, clientGoneErr(completed)
			}
			dst.Flush()
			parseSSELine(line, proto, &usage, &firstTokenAt, &completed)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return usage, firstTokenAt, nil // normal end of stream
			}
			if errors.Is(readErr, context.Canceled) || ctx.Err() != nil {
				return usage, firstTokenAt, clientGoneErr(completed)
			}
			return usage, firstTokenAt, fmt.Errorf("read upstream: %w", readErr)
		}
	}
}

// clientGoneErr classifies a client-side disconnect. If the stream already
// reached its protocol completion signal (end event / usage seen), the
// disconnect is benign drain-truncation (errClientGone); otherwise the stream
// was aborted mid-flight (errClientAborted).
func clientGoneErr(completed bool) error {
	if completed {
		return errClientGone
	}
	return errClientAborted
}

// sseSink 是 forwardSSE 的输出接口（http.ResponseWriter + Flush）。
type sseSink interface {
	io.Writer
	Flush()
}

// parseSSELine 解析单行 SSE。只看 data 行，按协议抽取 usage 与首 token 时刻。
func parseSSELine(line []byte, proto config.Protocol, usage **stats.Usage, firstTokenAt *time.Time, completed *bool) {
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
		parseChatSSE(data, usage, firstTokenAt, completed)
	case config.ProtoResponses:
		parseResponsesSSE(data, usage, firstTokenAt, completed)
	case config.ProtoAnthropic:
		parseAnthropicSSE(data, usage, firstTokenAt, completed)
	}
}

// parseChatSSE：usage 取末尾分片的 usage；TTFT 取首个含内容增量的分片。
func parseChatSSE(data string, usage **stats.Usage, firstTokenAt *time.Time, completed *bool) {
	var chunk protocol.ChatStreamChunk
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return
	}
	if chunk.Usage != nil {
		*usage = toStatsUsage(chunk.Usage)
		*completed = true // Chat usage only appears in the final chunk.
	}
	if firstTokenAt.IsZero() {
		for _, ch := range chunk.Choices {
			d := ch.Delta
			if d.Content != "" || d.ReasoningContent != "" || len(d.ToolCalls) > 0 {
				*firstTokenAt = time.Now()
				break
			}
		}
	}
	if !*completed {
		for _, ch := range chunk.Choices {
			if ch.FinishReason != nil {
				*completed = true
				break
			}
		}
	}
}

// responsesSSEEnvelope 是 Responses 事件流里我们关心的最小字段。
type responsesSSEEnvelope struct {
	Type     string `json:"type"`
	Response struct {
		Usage *protocol.ResponseUsage `json:"usage"`
	} `json:"response"`
}

// parseResponsesSSE：usage 取 response.completed 的 response.usage；
// TTFT 取首个 *.delta 事件时刻。
func parseResponsesSSE(data string, usage **stats.Usage, firstTokenAt *time.Time, completed *bool) {
	var ev responsesSSEEnvelope
	if json.Unmarshal([]byte(data), &ev) != nil {
		return
	}
	if ev.Response.Usage != nil {
		*usage = responseUsageToStats(ev.Response.Usage)
	}
	if firstTokenAt.IsZero() && strings.HasSuffix(ev.Type, ".delta") {
		*firstTokenAt = time.Now()
	}
	if ev.Type == "response.completed" {
		*completed = true
	}
}

func parseAnthropicSSE(data string, usage **stats.Usage, firstTokenAt *time.Time, completed *bool) {
	var ev protocol.AnthropicStreamEvent
	if json.Unmarshal([]byte(data), &ev) != nil {
		return
	}
	if ev.Message.Usage != nil {
		mergeAnthropicUsage(usage, ev.Message.Usage)
	}
	if ev.Usage != nil {
		mergeAnthropicUsage(usage, ev.Usage)
	}
	if firstTokenAt.IsZero() && ev.Type == "content_block_delta" && (ev.Delta.Type != "" || ev.Delta.Text != "") {
		*firstTokenAt = time.Now()
	}
	// message_start carries input usage but is NOT completion; only message_stop
	// marks the end of the stream.
	if ev.Type == "message_stop" {
		*completed = true
	}
}

func mergeAnthropicUsage(dst **stats.Usage, src *protocol.AnthropicUsage) {
	if src == nil {
		return
	}
	next := anthropicUsageToStats(src)
	if next == nil {
		return
	}
	if *dst == nil {
		*dst = next
		return
	}
	if next.InputTokens > 0 {
		(*dst).InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		(*dst).OutputTokens = next.OutputTokens
	}
	if next.CachedTokens > 0 {
		(*dst).CachedTokens = next.CachedTokens
	}
	if next.CacheCreationTokens > 0 {
		(*dst).CacheCreationTokens = next.CacheCreationTokens
	}
	(*dst).TotalTokens = (*dst).InputTokens + (*dst).OutputTokens
}
