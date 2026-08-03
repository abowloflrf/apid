package convert

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/abowloflrf/apid/protocol"
	"github.com/google/uuid"
)

// SSEWriter 是流式输出需要的最小接口（http.ResponseWriter + Flush）。
type SSEWriter interface {
	io.Writer
	Flush()
}

// StreamResult 是流式转换的终态结果，承载流末拿到的 usage、首 token 时刻，
// 以及供回写 reasoning 缓存的 assistant 信息。
type StreamResult struct {
	Usage         *protocol.ChatUsage
	FirstTokenAt  time.Time
	ToolCallIDs   []string // 所有 tool call 的 call_id，按 call_id 存 reasoning 用
	ReasoningText string   // 累积的 reasoning_content
	ContentText   string   // 累积的 assistant 文本，按内容指纹存 reasoning 用
}

// StreamChatToResponses 读取上游 Chat Completions 的 SSE 流，
// 转换为 Responses API 的事件流并写入 w。支持文本、reasoning 与工具调用三类增量。
//
// 产出的关键事件：
//
//	response.created
//	response.output_item.added / .done           (message / reasoning / function_call 三种 item)
//	response.output_text.delta / .done            (文本)
//	response.reasoning_summary_text.delta / .done (reasoning)
//	response.function_call_arguments.delta / .done(工具调用参数)
//	response.completed
func StreamChatToResponses(ctx context.Context, w SSEWriter, body io.Reader, model string, namespaces map[string]NamespacedTool) (*StreamResult, error) {
	respID := newStreamUUID()
	st := &streamState{
		w: w, model: model, tools: map[int]*toolState{},
		namespaces: namespaces, responseID: respID,
		createdAt: time.Now().Unix(),
	}

	// 开场两连发：response.created → response.in_progress，两者携带同一个
	// created_at 与 usage 占位，对齐 Responses schema(created_at 必填)，
	// 也满足按 in_progress 转状态机的严格客户端(如 Codex TUI)。
	st.emit("response.created", map[string]any{
		"type": "response.created", "response": st.openingResponse(),
	})
	st.emit("response.in_progress", map[string]any{
		"type": "response.in_progress", "response": st.openingResponse(),
	})
	if st.err != nil {
		return st.result()
	}

	// bufio.Reader (not Scanner) so a single long SSE line — e.g. a huge
	// tool-call argument delta — can't overflow a fixed token limit and kill
	// the stream. Same reading strategy as the raw forwarding path.
	reader := bufio.NewReader(body)
	for {
		line, readErr := reader.ReadString('\n')
		// Client gone (disconnect / shutdown): checked between read and process
		// so a line arriving together with the cancellation is not converted and
		// written to a client that can no longer receive it.
		if err := ctx.Err(); err != nil {
			return &StreamResult{Usage: st.usage, FirstTokenAt: st.firstTokenAt}, err
		}
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break
			}
			// Upstream reported an error mid-stream: emit response.failed to
			// the client and surface the error to the caller for logging/stats.
			if msg := parseStreamError(data); msg != "" {
				st.fail(msg)
				return &StreamResult{Usage: st.usage, FirstTokenAt: st.firstTokenAt},
					errors.Join(fmt.Errorf("upstream stream error: %s", msg), st.err)
			}
			if data != "" {
				st.consumeChunk(data)
				if st.err != nil {
					return st.result()
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			st.fail(readErr.Error())
			return &StreamResult{Usage: st.usage, FirstTokenAt: st.firstTokenAt}, errors.Join(readErr, st.err)
		}
	}

	st.finish()
	return st.result()
}

// consumeChunk parses one SSE data payload and feeds it into the stream state.
// Malformed payloads are skipped so one bad chunk never kills the stream.
func (st *streamState) consumeChunk(data string) {
	var chunk protocol.ChatStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	if chunk.Usage != nil {
		st.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
		st.finishReason = *fr
	}
	st.handleDelta(chunk.Choices[0].Delta)
}

// newStreamUUID 生成不带连字符的 UUID，用作流式响应的 ID。
func newStreamUUID() string {
	return "resp_" + strings.ReplaceAll(uuid.New().String(), "-", "")
}

func parseStreamError(data string) string {
	var probe struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &probe) != nil || probe.Error == nil {
		return ""
	}
	if probe.Error.Message != "" {
		return probe.Error.Message
	}
	return "upstream returned an error"
}

// streamState 累积一次流式响应的状态。
type streamState struct {
	w          SSEWriter
	model      string
	responseID string
	createdAt  int64
	usage      *protocol.ChatUsage
	err        error

	firstTokenAt time.Time
	finishReason string

	nextIndex int

	// reasoning item
	reasoningOpen  bool
	reasoningIndex int
	reasoningText  strings.Builder

	// message(文本) item
	textOpen  bool
	textIndex int
	textBuf   strings.Builder

	// function_call items
	tools     map[int]*toolState
	toolOrder []int

	namespaces map[string]NamespacedTool
}

type toolState struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	args        strings.Builder
}

func (st *streamState) reasoningItemID() string { return "rs_" + st.responseID }
func (st *streamState) textItemID() string      { return "msg_" + st.responseID }

// openingResponse 是 response.created / .in_progress 共用的瘦 response 对象。
// 带 created_at(schema 必填)；usage 此刻为 null，与官方一致，最终用量在 finish 回填。
func (st *streamState) openingResponse() map[string]any {
	return map[string]any{
		"id": st.responseID, "object": "response", "created_at": st.createdAt,
		"status": "in_progress", "model": st.model, "output": []any{},
		"usage": nil,
	}
}

func (st *streamState) functionCallItem(ts *toolState, status, args string) map[string]any {
	name, namespace := splitToolName(ts.name, st.namespaces)
	item := map[string]any{
		"id": ts.itemID, "type": "function_call", "status": status,
		"call_id": ts.callID, "name": name, "arguments": args,
	}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return item
}

func (st *streamState) handleDelta(d protocol.ChatChunkDelta) {
	if st.firstTokenAt.IsZero() &&
		(d.ReasoningContent != "" || d.Content != "" || len(d.ToolCalls) > 0) {
		st.firstTokenAt = time.Now()
	}
	if d.ReasoningContent != "" {
		st.handleReasoning(d.ReasoningContent)
	}
	if d.Content != "" {
		st.handleText(d.Content)
	}
	for _, tc := range d.ToolCalls {
		st.handleToolCall(tc)
	}
}

func (st *streamState) handleReasoning(delta string) {
	if !st.reasoningOpen {
		st.reasoningOpen = true
		st.reasoningIndex = st.nextIndex
		st.nextIndex++
		st.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": st.reasoningIndex,
			"item": map[string]any{
				"id": st.reasoningItemID(), "type": "reasoning",
				"status": "in_progress", "summary": []any{},
			},
		})
		// 开 summary part 槽位：与文本路径的 content_part.added 对称，
		// 严格客户端据此才会为 summary_index=0 建槽，否则丢弃后续 delta。
		st.emit("response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "item_id": st.reasoningItemID(),
			"output_index": st.reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	st.reasoningText.WriteString(delta)
	st.emit("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": st.reasoningItemID(),
		"output_index": st.reasoningIndex, "summary_index": 0, "delta": delta,
	})
}

func (st *streamState) handleText(delta string) {
	if !st.textOpen {
		st.textOpen = true
		st.textIndex = st.nextIndex
		st.nextIndex++
		st.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": st.textIndex,
			"item": map[string]any{
				"id": st.textItemID(), "type": "message",
				"status": "in_progress", "role": "assistant", "content": []any{},
			},
		})
		st.emit("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": st.textItemID(),
			"output_index": st.textIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	st.textBuf.WriteString(delta)
	st.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": st.textItemID(),
		"output_index": st.textIndex, "content_index": 0, "delta": delta,
	})
}

func (st *streamState) handleToolCall(tc protocol.ChatToolCall) {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	ts := st.tools[idx]
	if ts == nil {
		ts = &toolState{
			outputIndex: st.nextIndex,
			itemID:      "fc_" + st.responseID + "_" + strconv.Itoa(idx),
			callID:      tc.ID,
			name:        tc.Function.Name,
		}
		st.nextIndex++
		st.tools[idx] = ts
		st.toolOrder = append(st.toolOrder, idx)
		st.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": ts.outputIndex,
			"item": st.functionCallItem(ts, "in_progress", ""),
		})
	}
	if tc.ID != "" {
		ts.callID = tc.ID
	}
	if tc.Function.Name != "" {
		ts.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		ts.args.WriteString(tc.Function.Arguments)
		st.emit("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": ts.itemID,
			"output_index": ts.outputIndex, "delta": tc.Function.Arguments,
		})
	}
}

func (st *streamState) finish() {
	outputs := make([]any, st.nextIndex)

	if st.reasoningOpen {
		text := st.reasoningText.String()
		st.emit("response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": st.reasoningItemID(),
			"output_index": st.reasoningIndex, "summary_index": 0, "text": text,
		})
		// 关 summary part 槽位：与文本路径的 content_part.done 对称。
		st.emit("response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": st.reasoningItemID(),
			"output_index": st.reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		})
		item := map[string]any{
			"id": st.reasoningItemID(), "type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": text}},
		}
		st.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": st.reasoningIndex, "item": item,
		})
		outputs[st.reasoningIndex] = item
	}

	if st.textOpen {
		text := st.textBuf.String()
		st.emit("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": st.textItemID(),
			"output_index": st.textIndex, "content_index": 0, "text": text,
		})
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		st.emit("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": st.textItemID(),
			"output_index": st.textIndex, "content_index": 0, "part": part,
		})
		itemStatus := statusCompleted
		if s, _ := mapFinishReason(st.finishReason); s == statusIncomplete {
			itemStatus = statusIncomplete
		}
		item := map[string]any{
			"id": st.textItemID(), "type": "message", "status": itemStatus,
			"role": "assistant", "content": []any{part},
		}
		st.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": st.textIndex, "item": item,
		})
		outputs[st.textIndex] = item
	}

	for _, idx := range st.toolOrder {
		ts := st.tools[idx]
		args := ts.args.String()
		st.emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": ts.itemID,
			"output_index": ts.outputIndex, "arguments": args,
		})
		item := st.functionCallItem(ts, "completed", args)
		st.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": ts.outputIndex, "item": item,
		})
		outputs[ts.outputIndex] = item
	}

	status, incompleteReason := mapFinishReason(st.finishReason)
	resp := map[string]any{
		"id": st.responseID, "object": "response", "created_at": st.createdAt,
		"status": status, "model": st.model, "output": outputs,
	}
	if incompleteReason != "" {
		resp["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	if st.usage != nil {
		usage := map[string]any{
			"input_tokens":  st.usage.PromptTokens,
			"output_tokens": st.usage.CompletionTokens,
			"total_tokens":  st.usage.TotalTokens,
		}
		if d := st.usage.PromptTokensDetails; d != nil {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": d.CachedTokens}
		}
		if d := st.usage.CompletionTokensDetails; d != nil {
			usage["output_tokens_details"] = map[string]any{"reasoning_tokens": d.ReasoningTokens}
		}
		resp["usage"] = usage
	}
	event := "response.completed"
	if status == statusIncomplete {
		event = "response.incomplete"
	}
	st.emit(event, map[string]any{"type": event, "response": resp})
}

// result 从 streamState 构造 StreamResult，供调用方回写 reasoning 缓存。
func (st *streamState) result() (*StreamResult, error) {
	var callIDs []string
	for _, idx := range st.toolOrder {
		if ts := st.tools[idx]; ts.callID != "" {
			callIDs = append(callIDs, ts.callID)
		}
	}
	return &StreamResult{
		Usage:         st.usage,
		FirstTokenAt:  st.firstTokenAt,
		ToolCallIDs:   callIDs,
		ReasoningText: st.reasoningText.String(),
		ContentText:   st.textBuf.String(),
	}, st.err
}

func (st *streamState) fail(message string) {
	resp := map[string]any{
		"id": st.responseID, "object": "response", "created_at": st.createdAt,
		"status": statusFailed, "model": st.model, "output": []any{},
		"error": map[string]any{"code": "upstream_error", "message": message},
	}
	st.emit("response.failed", map[string]any{"type": "response.failed", "response": resp})
}

func (st *streamState) emit(event string, payload any) {
	if st.err != nil {
		return
	}
	st.err = emit(st.w, event, payload)
}

func emit(w SSEWriter, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	w.Flush()
	return nil
}
