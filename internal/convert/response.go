package convert

import (
	"strconv"

	"github.com/abowloflrf/apid/internal/types"
)

// ChatToResponses 把一个非流式 Chat Completions 响应转换为 Responses API 响应。
// 输出项顺序：reasoning -> message(文本) -> function_call(工具调用)。
// namespaces 是「扁平工具名 -> (命名空间, 本地名)」映射(见 ToolNamespaces)，用于把
// MCP 工具的 function_call 从上游扁平名拆回本地名 + namespace；非命名空间工具传 nil 即可。
func ChatToResponses(c *types.ChatResponse, namespaces map[string]NamespacedTool) *types.ResponsesResponse {
	id := stripPrefix(c.ID)

	var msg types.ChatMessage
	finishReason := ""
	if len(c.Choices) > 0 {
		msg = c.Choices[0].Message
		finishReason = c.Choices[0].FinishReason
	}

	// 由 finish_reason 决定整体状态：被截断(length)/被过滤(content_filter)时
	// 应报 incomplete，否则客户端会把残缺输出当成完整结果。
	status, incompleteReason := mapFinishReason(finishReason)
	// 被截断时，正在生成的 message 文本项本身也应标记为 incomplete。
	msgStatus := "completed"
	if status == statusIncomplete {
		msgStatus = statusIncomplete
	}

	output := make([]types.OutputItem, 0, 3)

	// 1) reasoning：上游若返回 reasoning_content，转为 reasoning 输出项。
	if msg.ReasoningContent != "" {
		output = append(output, types.OutputItem{
			Type:   "reasoning",
			ID:     "rs_" + id,
			Status: "completed",
			Summary: []types.SummaryText{
				{Type: "summary_text", Text: msg.ReasoningContent},
			},
		})
	}

	// 2) message：有文本内容时输出 output_text 消息项。
	if msg.Content != "" {
		output = append(output, types.OutputItem{
			Type:   "message",
			ID:     "msg_" + id,
			Status: msgStatus,
			Role:   "assistant",
			Content: []types.OutputContent{
				{Type: "output_text", Text: msg.Content, Annotations: []any{}},
			},
		})
	}

	// 3) function_call：每个工具调用转为一个 function_call 输出项。
	// 命名空间工具要把上游扁平名拆回本地名 + namespace(见 ToolNamespaces)。
	for i, tc := range msg.ToolCalls {
		name, namespace := splitToolName(tc.Function.Name, namespaces)
		output = append(output, types.OutputItem{
			Type:      "function_call",
			ID:        callItemID(id, i),
			Status:    "completed",
			CallID:    tc.ID,
			Name:      name,
			Arguments: tc.Function.Arguments,
			Namespace: namespace,
		})
	}

	resp := &types.ResponsesResponse{
		ID:        "resp_" + id,
		Object:    "response",
		CreatedAt: c.Created,
		Status:    status,
		Model:     c.Model,
		Output:    output,
	}
	if incompleteReason != "" {
		resp.IncompleteDetails = &types.IncompleteDetails{Reason: incompleteReason}
	}

	if c.Usage != nil {
		resp.Usage = &types.ResponseUsage{
			InputTokens:  c.Usage.PromptTokens,
			OutputTokens: c.Usage.CompletionTokens,
			TotalTokens:  c.Usage.TotalTokens,
		}
		if d := c.Usage.PromptTokensDetails; d != nil {
			resp.Usage.InputTokensDetails = &types.InputTokensDetails{CachedTokens: d.CachedTokens}
		}
		if d := c.Usage.CompletionTokensDetails; d != nil {
			resp.Usage.OutputTokensDetails = &types.OutputTokensDetails{ReasoningTokens: d.ReasoningTokens}
		}
	}

	return resp
}

// Responses API 的响应状态取值。
const (
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"
)

// mapFinishReason 把上游 Chat 的 finish_reason 映射为 Responses 的 status，
// 并在被截断/被过滤时给出 incomplete_details.reason。
//
//	stop / tool_calls / function_call / ""  -> completed（无 reason）
//	length                                  -> incomplete, max_output_tokens
//	content_filter                          -> incomplete, content_filter
//
// 其余未知值按 completed 处理（宁可不误报 incomplete）。
func mapFinishReason(reason string) (status, incompleteReason string) {
	switch reason {
	case "length":
		return statusIncomplete, "max_output_tokens"
	case "content_filter":
		return statusIncomplete, "content_filter"
	default:
		return statusCompleted, ""
	}
}

// splitToolName 把上游的扁平工具名拆成 (本地名, 命名空间)。
// 命中 namespaces 映射的(MCP 工具)返回本地名 + 命名空间；其余原样返回、namespace 为空。
func splitToolName(flat string, namespaces map[string]NamespacedTool) (name, namespace string) {
	if ref, ok := namespaces[flat]; ok {
		return ref.Name, ref.Namespace
	}
	return flat, ""
}

// callItemID 为第 i 个工具调用生成 function_call 输出项的 id。
func callItemID(base string, i int) string {
	return "fc_" + base + "_" + strconv.Itoa(i)
}

// stripPrefix 去掉上游 id 里常见的 "chatcmpl-" 前缀，方便拼成 resp_/msg_ 形式。
func stripPrefix(id string) string {
	const p = "chatcmpl-"
	if len(id) > len(p) && id[:len(p)] == p {
		return id[len(p):]
	}
	return id
}
