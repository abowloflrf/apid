package convert

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/abowloflrf/apid/internal/types"
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
	Usage         *types.ChatUsage
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
	}

	emit(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": respID, "object": "response", "status": "in_progress",
			"model": model, "output": []any{},
		},
	})

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return &StreamResult{Usage: st.usage, FirstTokenAt: st.firstTokenAt}, err
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		if msg := parseStreamError(data); msg != "" {
			log.Printf("upstream stream error: %s", msg)
			st.fail(msg)
			return &StreamResult{}, nil
		}
		var chunk types.ChatStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			st.usage = chunk.Usage
			cached := 0
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				cached = d.CachedTokens
			}
			log.Printf("stream usage: prompt_tokens=%d completion_tokens=%d total_tokens=%d cached_tokens=%d",
				chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens, cached)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
			st.finishReason = *fr
		}
		st.handleDelta(chunk.Choices[0].Delta)
	}
	if err := scanner.Err(); err != nil {
		st.fail(err.Error())
		return &StreamResult{Usage: st.usage, FirstTokenAt: st.firstTokenAt}, err
	}

	st.finish()
	return st.result()
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
	usage      *types.ChatUsage

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

func (st *streamState) handleDelta(d types.ChatChunkDelta) {
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
		emit(st.w, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": st.reasoningIndex,
			"item": map[string]any{
				"id": st.reasoningItemID(), "type": "reasoning",
				"status": "in_progress", "summary": []any{},
			},
		})
		// 开 summary part 槽位：与文本路径的 content_part.added 对称，
		// 严格客户端据此才会为 summary_index=0 建槽，否则丢弃后续 delta。
		emit(st.w, "response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "item_id": st.reasoningItemID(),
			"output_index": st.reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	st.reasoningText.WriteString(delta)
	emit(st.w, "response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": st.reasoningItemID(),
		"output_index": st.reasoningIndex, "summary_index": 0, "delta": delta,
	})
}

func (st *streamState) handleText(delta string) {
	if !st.textOpen {
		st.textOpen = true
		st.textIndex = st.nextIndex
		st.nextIndex++
		emit(st.w, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": st.textIndex,
			"item": map[string]any{
				"id": st.textItemID(), "type": "message",
				"status": "in_progress", "role": "assistant", "content": []any{},
			},
		})
		emit(st.w, "response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": st.textItemID(),
			"output_index": st.textIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	st.textBuf.WriteString(delta)
	emit(st.w, "response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": st.textItemID(),
		"output_index": st.textIndex, "content_index": 0, "delta": delta,
	})
}

func (st *streamState) handleToolCall(tc types.ChatToolCall) {
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
		emit(st.w, "response.output_item.added", map[string]any{
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
		emit(st.w, "response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": ts.itemID,
			"output_index": ts.outputIndex, "delta": tc.Function.Arguments,
		})
	}
}

func (st *streamState) finish() {
	outputs := make([]any, st.nextIndex)

	if st.reasoningOpen {
		text := st.reasoningText.String()
		emit(st.w, "response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": st.reasoningItemID(),
			"output_index": st.reasoningIndex, "summary_index": 0, "text": text,
		})
		// 关 summary part 槽位：与文本路径的 content_part.done 对称。
		emit(st.w, "response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": st.reasoningItemID(),
			"output_index": st.reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		})
		item := map[string]any{
			"id": st.reasoningItemID(), "type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": text}},
		}
		emit(st.w, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": st.reasoningIndex, "item": item,
		})
		outputs[st.reasoningIndex] = item
	}

	if st.textOpen {
		text := st.textBuf.String()
		emit(st.w, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": st.textItemID(),
			"output_index": st.textIndex, "content_index": 0, "text": text,
		})
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		emit(st.w, "response.content_part.done", map[string]any{
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
		emit(st.w, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": st.textIndex, "item": item,
		})
		outputs[st.textIndex] = item
	}

	for _, idx := range st.toolOrder {
		ts := st.tools[idx]
		args := ts.args.String()
		emit(st.w, "response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": ts.itemID,
			"output_index": ts.outputIndex, "arguments": args,
		})
		item := st.functionCallItem(ts, "completed", args)
		emit(st.w, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": ts.outputIndex, "item": item,
		})
		outputs[ts.outputIndex] = item
	}

	status, incompleteReason := mapFinishReason(st.finishReason)
	resp := map[string]any{
		"id": st.responseID, "object": "response", "status": status,
		"model": st.model, "output": outputs,
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
		resp["usage"] = usage
	}
	event := "response.completed"
	if status == statusIncomplete {
		event = "response.incomplete"
	}
	emit(st.w, event, map[string]any{"type": event, "response": resp})
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
	}, nil
}

func (st *streamState) fail(message string) {
	resp := map[string]any{
		"id": st.responseID, "object": "response", "status": statusFailed,
		"model": st.model, "output": []any{},
		"error": map[string]any{"code": "upstream_error", "message": message},
	}
	emit(st.w, "response.failed", map[string]any{"type": "response.failed", "response": resp})
}

func emit(w SSEWriter, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", b)
	w.Flush()
}
