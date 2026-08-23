// Package convert 实现 Responses API 与 Chat Completions API 之间的字段转换。
package convert

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/abowloflrf/apid/protocol"
)

// ReasoningSource 按 call_id 或 assistant 文本内容取回此前缓存的 reasoning_content，
// 由 internal/reasoning.Cache 实现；nil 表示不回填。
type ReasoningSource interface {
	ByCallID(callID string) string
	ByContent(content string) string
}

// ResponsesToChat 把一个 Responses API 请求转换为 Chat Completions 请求。
// src 用于回填 reasoning_content：Codex 重放历史时不保证带回上一轮的 reasoning，
// 而思考模型要求 assistant 的 reasoning_content 必须回传，故按 call_id 或
// assistant 文本指纹从网关缓存里取回；src 为 nil 时跳过回填。
//
// 同时返回「扁平工具名 -> (命名空间, 本地名)」映射(见 expandTools)，供响应方向
// 把 MCP 工具的 function_call 从上游扁平名拆回本地名 + namespace。
func ResponsesToChat(r *protocol.ResponsesRequest, src ReasoningSource) (*protocol.ChatRequest, map[string]NamespacedTool, error) {
	messages := make([]protocol.ChatMessage, 0, 4)

	// instructions 映射为 system 消息，放在最前面。
	if r.Instructions != "" {
		messages = append(messages, protocol.ChatMessage{Role: "system", Content: r.Instructions})
	}

	inputMsgs, err := parseInput(r.Input, src)
	if err != nil {
		return nil, nil, err
	}
	messages = append(messages, inputMsgs...)

	tools, namespaces := expandTools(r.Tools)
	chat := &protocol.ChatRequest{
		Model:             r.Model,
		Messages:          messages,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		MaxTokens:         r.MaxOutputTokens,
		Stream:            r.Stream,
		Tools:             tools,
		ToolChoice:        convertToolChoice(r.ToolChoice),
		ParallelToolCalls: r.ParallelToolCalls,
		ResponseFormat:    convertResponseFormat(r.Text),
	}

	if chat.Stream {
		chat.StreamOptions = &protocol.StreamOptions{IncludeUsage: true}
	}

	if r.Reasoning != nil && r.Reasoning.Effort != "" {
		chat.ReasoningEffort = r.Reasoning.Effort
	}

	return chat, namespaces, nil
}

func convertResponseFormat(text *protocol.ResponsesTextConfig) *protocol.ChatResponseFormat {
	if text == nil || text.Format == nil {
		return nil
	}
	format := text.Format
	chat := &protocol.ChatResponseFormat{Type: format.Type}
	if format.Type == "json_schema" {
		chat.JSONSchema = &protocol.ChatJSONSchema{
			Name:        format.Name,
			Description: format.Description,
			Schema:      format.Schema,
			Strict:      format.Strict,
		}
	}
	return chat
}

// NamespacedTool 记录一个命名空间工具拆解后的两部分。
type NamespacedTool struct {
	Namespace string
	Name      string
}

// expandTools 递归展开 Responses 工具定义，一次遍历得到两样东西：
//   - 发给上游的扁平 Chat 工具列表
//   - 「扁平名 -> (命名空间, 本地名)」映射
func expandTools(tools []protocol.ResponsesTool) ([]protocol.ChatTool, map[string]NamespacedTool) {
	var chat []protocol.ChatTool
	namespaces := make(map[string]NamespacedTool)

	var walk func(tools []protocol.ResponsesTool, prefix string)
	walk = func(tools []protocol.ResponsesTool, prefix string) {
		for _, t := range tools {
			switch {
			case t.Type == "namespace":
				walk(t.Tools, joinToolName(prefix, t.Name))
			case t.Type == "function" && t.Name != "":
				flat := joinToolName(prefix, t.Name)
				chat = append(chat, protocol.ChatTool{
					Type: "function",
					Function: protocol.ChatToolFunction{
						Name:        flat,
						Description: t.Description,
						Parameters:  ensureObjectSchema(t.Parameters),
						Strict:      t.Strict,
					},
				})
				if prefix != "" {
					namespaces[flat] = NamespacedTool{Namespace: prefix, Name: t.Name}
				}
			default:
				// convert has no injected logger; the process default is set in main.
				slog.Warn("skipping unsupported tool type", "type", t.Type)
			}
		}
	}
	walk(tools, "")

	if len(chat) == 0 {
		chat = nil
	}
	return chat, namespaces
}

func joinToolName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "__" + name
}

func ensureObjectSchema(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	if _, ok := m["type"]; ok {
		return raw
	}
	m["type"] = json.RawMessage(`"object"`)
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

func convertToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	if obj.Type == "function" && obj.Name != "" {
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": obj.Name},
		})
		return b
	}
	switch obj.Type {
	case "auto":
		return json.RawMessage(`"auto"`)
	case "none":
		return json.RawMessage(`"none"`)
	case "required", "tool", "any", "function":
		return json.RawMessage(`"required"`)
	}
	return raw
}

// parseInput 解析 Responses 的 input 字段。
// src 用于按 call_id / assistant 文本指纹回填 reasoning_content；nil 时跳过。
func parseInput(raw json.RawMessage, src ReasoningSource) ([]protocol.ChatMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []protocol.ChatMessage{{Role: "user", Content: s}}, nil
	}

	var items []protocol.InputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}

	out := make([]protocol.ChatMessage, 0, len(items))
	// pendingReasoning 暂存上一条 reasoning 输入项的摘要，仅作缓存未命中时的兜底；
	// reasoning 回传优先取网关缓存里的原文（Codex 重放的 reasoning 不可靠）。
	pendingReasoning := ""
	// toolMsg 指向当前正在累积工具调用的 assistant 消息，连续 function_call 合并进同一条。
	var toolMsg *protocol.ChatMessage

	for _, it := range items {
		if it.Type != "function_call" {
			toolMsg = nil
		}
		switch it.Type {
		case "function_call":
			name := it.Name
			if it.Namespace != "" {
				name = joinToolName(it.Namespace, it.Name)
			}
			call := protocol.ChatToolCall{
				ID:   it.CallID,
				Type: "function",
				Function: protocol.ChatToolCallFunction{
					Name:      name,
					Arguments: it.Arguments,
				},
			}
			if toolMsg == nil {
				// 新开一轮并行工具调用，reasoning_content 挂在首条上：
				// 优先取缓存里按 call_id 存的原文，未命中再用 input 摘要兜底。
				rc := lookupCall(src, it.CallID)
				if rc == "" {
					rc = pendingReasoning
				}
				out = append(out, protocol.ChatMessage{
					Role:             "assistant",
					ReasoningContent: rc,
				})
				pendingReasoning = ""
				toolMsg = &out[len(out)-1]
			}
			toolMsg.ToolCalls = append(toolMsg.ToolCalls, call)

		case "function_call_output":
			out = append(out, protocol.ChatMessage{
				Role:       "tool",
				ToolCallID: it.CallID,
				Content:    extractText(it.Output),
			})

		case "reasoning":
			pendingReasoning = summaryText(it.Summary)

		default: // "" 或 "message"
			role := mapRole(it.Role)
			content := extractText(it.Content)
			msg := protocol.ChatMessage{Role: role, Content: content}
			if role == "assistant" {
				// 按 assistant 文本指纹取回原文，未命中用摘要兜底。
				rc := lookupContent(src, content)
				if rc == "" {
					rc = pendingReasoning
				}
				msg.ReasoningContent = rc
				pendingReasoning = ""
			}
			out = append(out, msg)
		}
	}

	return out, nil
}

// lookupCall / lookupContent 是 ReasoningSource 的 nil 安全包装。
func lookupCall(src ReasoningSource, callID string) string {
	if src == nil {
		return ""
	}
	return src.ByCallID(callID)
}

func lookupContent(src ReasoningSource, content string) string {
	if src == nil || content == "" {
		return ""
	}
	return src.ByContent(content)
}

// summaryText 把 reasoning 项的 summary 块拼接成纯文本。
func summaryText(summary []protocol.SummaryText) string {
	var b strings.Builder
	for _, s := range summary {
		b.WriteString(s.Text)
	}
	return b.String()
}

// mapRole 把 Responses 消息角色映射为 Chat Completions 支持的角色。
func mapRole(role string) string {
	if role == "developer" {
		return "system"
	}
	return role
}

// extractText 从 content / output 字段提取纯文本，支持字符串或内容块数组两种形态。
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []protocol.InputContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// FixSystemOrdering 修正 system/developer 消息被 Codex 插在 function_call 和
// function_call_output 之间导致的顺序问题。Chat Completions API 要求
// assistant[tool_calls] → tool 必须连续，中间不能有 system 消息。
// 被插队的 system 消息移到 messages[0]（替换已有 system 或插在最前面）。
func FixSystemOrdering(messages []protocol.ChatMessage) []protocol.ChatMessage {
	if len(messages) <= 1 {
		return messages
	}

	// 从前往后扫描，把 index > 0 且 role == "system" 的消息移到最前面。
	var cleaned []protocol.ChatMessage
	for i, m := range messages {
		if i > 0 && m.Role == "system" {
			// 替换 messages[0] 如果它已经是 system，否则 insert。
			if len(cleaned) > 0 && cleaned[0].Role == "system" {
				cleaned[0] = m
			} else {
				cleaned = append([]protocol.ChatMessage{m}, cleaned...)
			}
		} else {
			cleaned = append(cleaned, m)
		}
	}
	return cleaned
}
