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
)

// SSEWriter 是流式输出需要的最小接口（http.ResponseWriter + Flush）。
type SSEWriter interface {
	io.Writer
	Flush()
}

// StreamResult 是流式转换的终态结果，承载流末拿到的 usage 与首 token 时刻。
// 上游未发送 usage 分片时 Usage 为 nil；失败 / 中途出错同样为 nil。
// FirstTokenAt 是收到上游第一个有内容增量(文本 / reasoning / tool_call)的时刻，
// 供调用方计算 TTFT；整条流没有任何内容增量时为零值(IsZero() 为 true)。
type StreamResult struct {
	Usage        *types.ChatUsage
	FirstTokenAt time.Time
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
// namespaces 是「扁平工具名 -> (命名空间, 本地名)」映射(见 ToolNamespaces)，用于把
// MCP 工具的 function_call 事件从上游扁平名拆回本地名 + namespace；无命名空间工具传 nil。
func StreamChatToResponses(ctx context.Context, w SSEWriter, body io.Reader, model string, namespaces map[string]NamespacedTool) (*StreamResult, error) {
	st := &streamState{w: w, model: model, tools: map[int]*toolState{}, namespaces: namespaces}

	emit(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": respStreamID, "object": "response", "status": "in_progress",
			"model": model, "output": []any{},
		},
	})

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		// Client gone (disconnect / shutdown): stop converting and reading upstream.
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
		// 上游可能在流中途以 {"error":{...}} 形式报错(vLLM / 兼容网关常见)。
		// 此时已经发过 response.created，必须补一个 response.failed，
		// 否则客户端等不到收尾事件会挂死或超时。
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
		// finish_reason 通常在末尾一个 delta 为空的分片上携带，记录下来供收尾判断状态。
		if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
			st.finishReason = *fr
		}
		st.handleDelta(chunk.Choices[0].Delta)
	}
	if err := scanner.Err(); err != nil {
		// 读流出错(如单行超出缓冲上限)同样补发 response.failed 再返回。
		st.fail(err.Error())
		return &StreamResult{Usage: st.usage, FirstTokenAt: st.firstTokenAt}, err
	}

	st.finish()
	return &StreamResult{Usage: st.usage, FirstTokenAt: st.firstTokenAt}, nil
}

// parseStreamError 探测一行 SSE data 是否是上游的错误对象，是则返回错误信息。
// 普通分片没有 "error" 键，probe.Error 为 nil，返回空串。
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

const respStreamID = "resp_stream"

// streamState 累积一次流式响应的状态。
type streamState struct {
	w     SSEWriter
	model string
	usage *types.ChatUsage

	// 收到第一个有内容增量的时刻，用于 TTFT；后续增量不再覆盖。
	firstTokenAt time.Time

	// 上游最后一个非空 finish_reason，收尾时映射为 Responses 状态。
	finishReason string

	nextIndex int // 下一个 output item 的 output_index

	// reasoning item
	reasoningOpen  bool
	reasoningIndex int
	reasoningText  strings.Builder

	// message(文本) item
	textOpen  bool
	textIndex int
	textBuf   strings.Builder

	// function_call items，按上游 tool_call 的 index 归类
	tools     map[int]*toolState
	toolOrder []int // 保持出现顺序

	// 扁平工具名 -> (命名空间, 本地名)，用于把 function_call 拆回本地名 + namespace。
	namespaces map[string]NamespacedTool
}

type toolState struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	args        strings.Builder
}

func (st *streamState) reasoningItemID() string { return "rs_stream" }
func (st *streamState) textItemID() string      { return "msg_stream" }

// functionCallItem 组装一个 function_call item map。命名空间工具(MCP)要把上游扁平名
// 拆回本地名 + namespace，缺 namespace 字段 Codex 会路由失败(unsupported call)。
// added/done 共用，保证字段一致。
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

// handleDelta 处理一个 chat 分片的 delta。
func (st *streamState) handleDelta(d types.ChatChunkDelta) {
	// 第一个携带实际内容的增量决定 TTFT；空 delta(仅 finish_reason 等)不计入。
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
			itemID:      "fc_stream_" + strconv.Itoa(idx),
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
	// id / name 可能在后续分片才补全。
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

// finish 发送收尾事件，并组装 response.completed。
func (st *streamState) finish() {
	outputs := make([]any, st.nextIndex)

	if st.reasoningOpen {
		text := st.reasoningText.String()
		emit(st.w, "response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": st.reasoningItemID(),
			"output_index": st.reasoningIndex, "summary_index": 0, "text": text,
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
		// 被截断时文本项标记 incomplete，与顶层状态一致。
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

	// 由 finish_reason 决定收尾状态与事件名：被截断/被过滤时发 response.incomplete。
	status, incompleteReason := mapFinishReason(st.finishReason)
	resp := map[string]any{
		"id": respStreamID, "object": "response", "status": status,
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

// fail 在流中途出错时补发 response.failed 事件，让客户端有明确的收尾。
func (st *streamState) fail(message string) {
	resp := map[string]any{
		"id": respStreamID, "object": "response", "status": statusFailed,
		"model": st.model, "output": []any{},
		"error": map[string]any{"code": "upstream_error", "message": message},
	}
	emit(st.w, "response.failed", map[string]any{"type": "response.failed", "response": resp})
}

// emit 写一条 SSE 事件（event: 行 + data: 行 + 空行）并立即 flush。
func emit(w SSEWriter, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", b)
	w.Flush()
}
