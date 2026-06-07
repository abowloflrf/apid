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

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/stats"
	"github.com/abowloflrf/apid/internal/types"
)

// forwardSSE streams upstream SSE to the client verbatim, tee-parsing usage and
// the first-token time. Parsing failures are swallowed (never affect forwarding),
// but a non-EOF read error or a client write error is returned so stats can flag
// a truncated stream. Returns end-of-stream usage and first content-delta time.
func forwardSSE(ctx context.Context, dst sseSink, src io.Reader, proto config.Protocol) (*stats.Usage, time.Time, error) {
	reader := bufio.NewReader(src)
	var usage *stats.Usage
	var firstTokenAt time.Time

	for {
		// Client gone (disconnect / shutdown): stop reading upstream.
		if err := ctx.Err(); err != nil {
			return usage, firstTokenAt, err
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := dst.Write(line); werr != nil {
				// Client gone: stop reading upstream and flag it.
				return usage, firstTokenAt, fmt.Errorf("write client: %w", werr)
			}
			dst.Flush()
			parseSSELine(line, proto, &usage, &firstTokenAt)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return usage, firstTokenAt, nil // normal end of stream
			}
			return usage, firstTokenAt, fmt.Errorf("read upstream: %w", readErr)
		}
	}
}

// sseSink 是 forwardSSE 的输出接口（http.ResponseWriter + Flush）。
type sseSink interface {
	io.Writer
	Flush()
}

// parseSSELine 解析单行 SSE。只看 data 行，按协议抽取 usage 与首 token 时刻。
func parseSSELine(line []byte, proto config.Protocol, usage **stats.Usage, firstTokenAt *time.Time) {
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
		parseChatSSE(data, usage, firstTokenAt)
	case config.ProtoResponses:
		parseResponsesSSE(data, usage, firstTokenAt)
	}
}

// parseChatSSE：usage 取末尾分片的 usage；TTFT 取首个含内容增量的分片。
func parseChatSSE(data string, usage **stats.Usage, firstTokenAt *time.Time) {
	var chunk types.ChatStreamChunk
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return
	}
	if chunk.Usage != nil {
		*usage = toStatsUsage(chunk.Usage)
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
}

// responsesSSEEnvelope 是 Responses 事件流里我们关心的最小字段。
type responsesSSEEnvelope struct {
	Type     string `json:"type"`
	Response struct {
		Usage *types.ResponseUsage `json:"usage"`
	} `json:"response"`
}

// parseResponsesSSE：usage 取 response.completed 的 response.usage；
// TTFT 取首个 *.delta 事件时刻。
func parseResponsesSSE(data string, usage **stats.Usage, firstTokenAt *time.Time) {
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
}
