package convert

import (
	"strconv"

	"github.com/abowloflrf/apid/protocol"
)

// ChatToResponses 把一个非流式 Chat Completions 响应转换为 Responses API 响应。
// 输出项顺序：reasoning -> message(文本) -> function_call(工具调用)。
// namespaces maps Chat tool names back to their original Responses identity,
// including namespace and custom tool type.
func ChatToResponses(c *protocol.ChatResponse, namespaces map[string]NamespacedTool) *protocol.ResponsesResponse {
	id := stripPrefix(c.ID)

	var msg protocol.ChatMessage
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

	output := make([]protocol.OutputItem, 0, 3)

	// 1) reasoning：上游若返回 reasoning_content，转为 reasoning 输出项。
	if msg.ReasoningContent != "" {
		output = append(output, protocol.OutputItem{
			Type:   "reasoning",
			ID:     "rs_" + id,
			Status: "completed",
			Summary: []protocol.SummaryText{
				{Type: "summary_text", Text: msg.ReasoningContent},
			},
		})
	}

	// 2) message：有文本内容或 refusal 时输出 message 项。
	// 上游拒绝(content_filter 等)走 refusal 字段，映射为 refusal 内容块，
	// 否则客户端只看到一条空 message、不知为何无输出。
	if msg.Content != "" || msg.Refusal != "" {
		content := make([]protocol.OutputContent, 0, 2)
		if msg.Content != "" {
			content = append(content, protocol.OutputContent{
				Type: "output_text", Text: msg.Content, Annotations: []any{},
			})
		}
		if msg.Refusal != "" {
			content = append(content, protocol.OutputContent{Type: "refusal", Refusal: msg.Refusal})
		}
		output = append(output, protocol.OutputItem{
			Type:    "message",
			ID:      "msg_" + id,
			Status:  msgStatus,
			Role:    "assistant",
			Content: content,
		})
	}

	// 3) Restore each Chat tool call to its original Responses tool type.
	for i, tc := range msg.ToolCalls {
		ref := resolveTool(tc.Function.Name, namespaces)
		if ref.Type == "custom" {
			input := unwrapCustomToolInput(tc.Function.Arguments)
			output = append(output, protocol.OutputItem{
				Type:      "custom_tool_call",
				ID:        customCallItemID(id, i),
				Status:    "completed",
				CallID:    tc.ID,
				Name:      ref.Name,
				Input:     &input,
				Namespace: ref.Namespace,
			})
			continue
		}
		output = append(output, protocol.OutputItem{
			Type:      "function_call",
			ID:        callItemID(id, i),
			Status:    "completed",
			CallID:    tc.ID,
			Name:      ref.Name,
			Arguments: tc.Function.Arguments,
			Namespace: ref.Namespace,
		})
	}

	resp := &protocol.ResponsesResponse{
		ID:        "resp_" + id,
		Object:    "response",
		CreatedAt: c.Created,
		Status:    status,
		Model:     c.Model,
		Output:    output,
	}
	if incompleteReason != "" {
		resp.IncompleteDetails = &protocol.IncompleteDetails{Reason: incompleteReason}
	}

	if c.Usage != nil {
		resp.Usage = &protocol.ResponseUsage{
			InputTokens:  c.Usage.PromptTokens,
			OutputTokens: c.Usage.CompletionTokens,
			TotalTokens:  c.Usage.TotalTokens,
		}
		if d := c.Usage.PromptTokensDetails; d != nil {
			resp.Usage.InputTokensDetails = &protocol.InputTokensDetails{CachedTokens: d.CachedTokens}
		}
		if d := c.Usage.CompletionTokensDetails; d != nil {
			resp.Usage.OutputTokensDetails = &protocol.OutputTokensDetails{ReasoningTokens: d.ReasoningTokens}
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
	ref := resolveTool(flat, namespaces)
	return ref.Name, ref.Namespace
}

func resolveTool(flat string, tools map[string]NamespacedTool) NamespacedTool {
	if ref, ok := tools[flat]; ok {
		if ref.Type == "" {
			ref.Type = "function"
		}
		return ref
	}
	return NamespacedTool{Type: "function", Name: flat}
}

// callItemID 为第 i 个工具调用生成 function_call 输出项的 id。
func callItemID(base string, i int) string {
	return "fc_" + base + "_" + strconv.Itoa(i)
}

func customCallItemID(base string, i int) string {
	return "ctc_" + base + "_" + strconv.Itoa(i)
}

// stripPrefix 去掉上游 id 里常见的 "chatcmpl-" 前缀，方便拼成 resp_/msg_ 形式。
func stripPrefix(id string) string {
	const p = "chatcmpl-"
	if len(id) > len(p) && id[:len(p)] == p {
		return id[len(p):]
	}
	return id
}
