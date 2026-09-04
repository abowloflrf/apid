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
//	response.output_item.added / .done           (message / reasoning / function_call / custom_tool_call / tool_search_call)
//	response.output_text.delta / .done            (文本)
//	response.reasoning_summary_text.delta / .done (reasoning)
//	response.function_call_arguments.delta / .done(工具调用参数)
//	response.custom_tool_call_input.delta / .done (custom tool input)
//	response.completed
func StreamChatToResponses(ctx context.Context, w SSEWriter, body io.Reader, model string, tools ToolContext) (*StreamResult, error) {
	respID := newStreamUUID()
	st := &streamState{
		w: w, model: model, tools: map[int]*toolState{},
		toolContext: tools, responseID: respID,
		createdAt: time.Now().Unix(),
		outputs:   make([]any, 0),
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
	sawDone := false
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
				sawDone = true
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

	if !sawDone && st.finishReason == "" {
		st.failConversion(errors.New("upstream stream closed before [DONE]"))
		return st.result()
	}
	if st.reasoningText.Len() > 0 && st.textBuf.Len() == 0 && len(st.tools) == 0 {
		st.failConversion(errors.New("upstream stream ended with reasoning but no answer or tool call"))
		return st.result()
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
	sequence   int

	firstTokenAt time.Time
	finishReason string

	nextIndex int
	outputs   []any

	// reasoning item
	reasoningOpen    bool
	reasoningStarted bool
	reasoningIndex   int
	reasoningText    strings.Builder

	// message(文本) item
	textOpen    bool
	textStarted bool
	textIndex   int
	textBuf     strings.Builder

	// function_call items
	tools     map[int]*toolState
	toolOrder []int

	toolContext ToolContext
}

type toolState struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	added       bool
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
		"background": false, "error": nil, "incomplete_details": nil, "usage": nil,
	}
}

func (st *streamState) toolCallItem(ts *toolState, status, args string) map[string]any {
	ref := resolveTool(ts.name, st.toolContext)
	if ref.Kind == "custom" {
		item := map[string]any{
			"id": ts.itemID, "type": "custom_tool_call", "status": status,
			"call_id": ts.callID, "name": ref.Name, "input": unwrapCustomToolInput(args),
		}
		if ref.Namespace != "" {
			item["namespace"] = ref.Namespace
		}
		return item
	}
	if ref.Kind == "tool_search" {
		execution := ref.Execution
		if execution == "" {
			execution = "client"
		}
		return map[string]any{
			"id": ts.itemID, "type": "tool_search_call", "status": status,
			"call_id": ts.callID, "execution": execution,
			"arguments": json.RawMessage(normalizeToolSearchArguments(args)),
		}
	}
	item := map[string]any{
		"id": ts.itemID, "type": "function_call", "status": status,
		"call_id": ts.callID, "name": ref.Name, "arguments": args,
	}
	if ref.Namespace != "" {
		item["namespace"] = ref.Namespace
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
	if st.err != nil {
		return
	}
	if d.Content != "" {
		st.handleText(d.Content)
	}
	if st.err != nil {
		return
	}
	for _, tc := range d.ToolCalls {
		st.handleToolCall(tc)
		if st.err != nil {
			return
		}
	}
}

func (st *streamState) handleReasoning(delta string) {
	if st.reasoningStarted && !st.reasoningOpen {
		st.failConversion(errors.New("reasoning delta arrived after the reasoning item was closed"))
		return
	}
	if st.textStarted || len(st.tools) > 0 {
		st.failConversion(errors.New("reasoning delta arrived after answer or tool output started"))
		return
	}
	if !st.reasoningOpen {
		st.reasoningOpen = true
		st.reasoningStarted = true
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
	if len(st.tools) > 0 {
		st.failConversion(errors.New("text delta arrived after tool output started"))
		return
	}
	st.finishReasoning()
	if st.err != nil {
		return
	}
	if st.textStarted && !st.textOpen {
		st.failConversion(errors.New("text delta arrived after the message item was closed"))
		return
	}
	if !st.textOpen {
		st.textOpen = true
		st.textStarted = true
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
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}},
		})
	}
	st.textBuf.WriteString(delta)
	st.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": st.textItemID(),
		"output_index": st.textIndex, "content_index": 0, "delta": delta, "logprobs": []any{},
	})
}

func (st *streamState) handleToolCall(tc protocol.ChatToolCall) {
	st.finishReasoning()
	st.finishText()
	if st.err != nil {
		return
	}
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	ts := st.tools[idx]
	if ts == nil {
		ts = &toolState{
			outputIndex: st.nextIndex,
			callID:      tc.ID,
			name:        tc.Function.Name,
		}
		st.nextIndex++
		st.tools[idx] = ts
		st.toolOrder = append(st.toolOrder, idx)
	}
	if tc.ID != "" {
		ts.callID = tc.ID
	}
	if tc.Function.Name != "" {
		ts.name = tc.Function.Name
	}
	args := tc.Function.Arguments
	if args != "" {
		ts.args.WriteString(args)
	}
	started := st.startTool(ts, idx)
	if st.err != nil {
		return
	}
	if args != "" && ts.added && !started {
		if resolveTool(ts.name, st.toolContext).Kind == "function" {
			st.emit("response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": ts.itemID,
				"output_index": ts.outputIndex, "delta": args,
			})
		}
	}
}

func (st *streamState) startTool(ts *toolState, idx int) bool {
	if ts.added || ts.name == "" || ts.callID == "" {
		return false
	}
	itemPrefix := "fc_"
	switch resolveTool(ts.name, st.toolContext).Kind {
	case "custom":
		itemPrefix = "ctc_"
	case "tool_search":
		itemPrefix = "tsc_"
	}
	ts.itemID = itemPrefix + st.responseID + "_" + strconv.Itoa(idx)
	ts.added = true
	st.emit("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": ts.outputIndex,
		"item": st.toolCallItem(ts, "in_progress", ""),
	})
	if st.err == nil && ts.args.Len() > 0 && resolveTool(ts.name, st.toolContext).Kind == "function" {
		st.emit("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": ts.itemID,
			"output_index": ts.outputIndex, "delta": ts.args.String(),
		})
	}
	return true
}

func (st *streamState) finish() {
	st.finishReasoning()
	st.finishText()
	if st.err != nil {
		return
	}

	for _, idx := range st.toolOrder {
		ts := st.tools[idx]
		if ts.name == "" {
			st.failConversion(fmt.Errorf("upstream tool call at index %d is missing name", idx))
			return
		}
		if ts.callID == "" {
			ts.callID = "call_" + strings.TrimPrefix(st.responseID, "resp_") + "_" + strconv.Itoa(idx)
		}
		st.startTool(ts, idx)
		if st.err != nil {
			return
		}
		args := ts.args.String()
		switch resolveTool(ts.name, st.toolContext).Kind {
		case "custom":
			input := unwrapCustomToolInput(args)
			if input != "" {
				st.emit("response.custom_tool_call_input.delta", map[string]any{
					"type": "response.custom_tool_call_input.delta", "item_id": ts.itemID,
					"output_index": ts.outputIndex, "delta": input,
				})
			}
			st.emit("response.custom_tool_call_input.done", map[string]any{
				"type": "response.custom_tool_call_input.done", "item_id": ts.itemID,
				"output_index": ts.outputIndex, "input": input,
			})
		case "function":
			st.emit("response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": ts.itemID,
				"output_index": ts.outputIndex, "arguments": args,
			})
		}
		item := st.toolCallItem(ts, "completed", args)
		st.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": ts.outputIndex, "item": item,
		})
		st.storeOutput(ts.outputIndex, item)
	}
	if st.err != nil {
		return
	}

	status, incompleteReason := mapFinishReason(st.finishReason)
	resp := map[string]any{
		"id": st.responseID, "object": "response", "created_at": st.createdAt,
		"status": status, "model": st.model, "output": st.outputs,
		"background": false, "error": nil, "incomplete_details": nil, "usage": nil,
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

func (st *streamState) finishReasoning() {
	if !st.reasoningOpen {
		return
	}
	text := st.reasoningText.String()
	st.emit("response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "item_id": st.reasoningItemID(),
		"output_index": st.reasoningIndex, "summary_index": 0, "text": text,
	})
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
	if st.err != nil {
		return
	}
	st.reasoningOpen = false
	st.storeOutput(st.reasoningIndex, item)
}

func (st *streamState) finishText() {
	if !st.textOpen {
		return
	}
	text := st.textBuf.String()
	st.emit("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": st.textItemID(),
		"output_index": st.textIndex, "content_index": 0, "text": text, "logprobs": []any{},
	})
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
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
	if st.err != nil {
		return
	}
	st.textOpen = false
	st.storeOutput(st.textIndex, item)
}

func (st *streamState) storeOutput(index int, item any) {
	for len(st.outputs) <= index {
		st.outputs = append(st.outputs, nil)
	}
	st.outputs[index] = item
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
		"background": false, "incomplete_details": nil, "usage": nil,
		"error": map[string]any{"code": "upstream_error", "message": message},
	}
	st.emit("response.failed", map[string]any{"type": "response.failed", "response": resp})
}

func (st *streamState) failConversion(err error) {
	st.fail(err.Error())
	st.err = errors.Join(err, st.err)
}

func (st *streamState) emit(event string, payload map[string]any) {
	if st.err != nil {
		return
	}
	st.sequence++
	payload["sequence_number"] = st.sequence
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
