// Package convert 实现 Responses API 与 Chat Completions API 之间的字段转换。
package convert

import (
	"bytes"
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
// The returned request-scoped ToolContext restores namespace and tool kinds
// when the Chat response is converted back to Responses.
func ResponsesToChat(r *protocol.ResponsesRequest, src ReasoningSource) (*protocol.ChatRequest, ToolContext, error) {
	messages := make([]protocol.ChatMessage, 0, 4)

	// instructions 映射为 system 消息，放在最前面。
	if r.Instructions != "" {
		messages = append(messages, protocol.ChatMessage{Role: "system", Content: r.Instructions})
	}

	dynamicTools, err := inputToolDeclarations(r.Input)
	if err != nil {
		return nil, nil, err
	}
	allTools := make([]protocol.ResponsesTool, 0, len(r.Tools)+len(dynamicTools))
	allTools = append(allTools, r.Tools...)
	allTools = append(allTools, dynamicTools...)
	tools, toolContext := expandTools(allTools)

	inputMsgs, err := parseInput(r.Input, src)
	if err != nil {
		return nil, nil, err
	}
	messages = append(messages, inputMsgs...)

	chat := &protocol.ChatRequest{
		Model:          r.Model,
		Messages:       messages,
		Temperature:    r.Temperature,
		TopP:           r.TopP,
		MaxTokens:      r.MaxOutputTokens,
		Stream:         r.Stream,
		Tools:          tools,
		ResponseFormat: convertResponseFormat(r.Text),
	}
	if len(tools) > 0 {
		chat.ToolChoice = convertToolChoice(r.ToolChoice)
		chat.ParallelToolCalls = r.ParallelToolCalls
	}

	if chat.Stream {
		chat.StreamOptions = &protocol.StreamOptions{IncludeUsage: true}
	}

	if r.Reasoning != nil && r.Reasoning.Effort != "" {
		chat.ReasoningEffort = r.Reasoning.Effort
	}

	return chat, toolContext, nil
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

// ToolSpec records the original Responses identity of a converted Chat tool.
type ToolSpec struct {
	Kind      string
	Namespace string
	Name      string
	Execution string
}

// ToolContext maps each emitted Chat function name back to its Responses tool.
// It also serves as the first-wins set when top-level and dynamic declarations merge.
type ToolContext map[string]ToolSpec

// expandTools 递归展开 Responses 工具定义，一次遍历得到两样东西：
//   - 发给上游的扁平 Chat 工具列表
//   - 「扁平名 -> (命名空间, 本地名)」映射
func expandTools(tools []protocol.ResponsesTool) ([]protocol.ChatTool, ToolContext) {
	var chat []protocol.ChatTool
	toolContext := make(ToolContext)
	add := func(flat string, spec ToolSpec, tool protocol.ChatTool) {
		if flat == "" {
			return
		}
		if _, exists := toolContext[flat]; exists {
			return
		}
		toolContext[flat] = spec
		chat = append(chat, tool)
	}

	var walk func(tools []protocol.ResponsesTool, prefix string)
	walk = func(tools []protocol.ResponsesTool, prefix string) {
		for _, t := range tools {
			switch {
			case t.Type == "namespace":
				walk(t.Tools, joinToolName(prefix, t.Name))
			case t.Type == "function" && t.Name != "":
				flat := joinToolName(prefix, t.Name)
				add(flat, ToolSpec{Kind: "function", Namespace: prefix, Name: t.Name}, protocol.ChatTool{
					Type: "function",
					Function: protocol.ChatToolFunction{
						Name:        flat,
						Description: t.Description,
						Parameters:  ensureObjectSchema(t.Parameters),
						Strict:      t.Strict,
					},
				})
			case t.Type == "custom" && t.Name != "":
				flat := joinToolName(prefix, t.Name)
				add(flat, ToolSpec{Kind: "custom", Namespace: prefix, Name: t.Name}, protocol.ChatTool{
					Type: "function",
					Function: protocol.ChatToolFunction{
						Name:        flat,
						Description: customToolDescription(t),
						Parameters:  customToolParameters(),
					},
				})
			case t.Type == "tool_search":
				parameters := t.Parameters
				if len(parameters) == 0 || string(parameters) == "null" {
					parameters = toolSearchParameters()
				} else {
					parameters = ensureObjectSchema(parameters)
				}
				description := t.Description
				if description == "" {
					description = "Search and load tools available to the client for the current task."
				}
				add("tool_search", ToolSpec{Kind: "tool_search", Name: "tool_search", Execution: "client"}, protocol.ChatTool{
					Type: "function",
					Function: protocol.ChatToolFunction{
						Name:        "tool_search",
						Description: description,
						Parameters:  parameters,
					},
				})
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
	return chat, toolContext
}

func toolSearchParameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`)
}

func customToolParameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`)
}

func customToolDescription(tool protocol.ResponsesTool) string {
	format := strings.TrimSpace(string(tool.Format))
	if format == "" || format == "null" {
		return tool.Description
	}
	if tool.Description == "" {
		return "Custom tool input format: " + format
	}
	return tool.Description + "\n\nCustom tool input format: " + format
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
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	if (obj.Type == "function" || obj.Type == "custom") && obj.Name != "" {
		name := joinToolName(obj.Namespace, obj.Name)
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": name},
		})
		return b
	}
	if obj.Type == "tool_search" {
		return json.RawMessage(`{"type":"function","function":{"name":"tool_search"}}`)
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

// inputToolDeclarations collects tools made available by earlier search results
// and by Responses-compatible additional_tools input items.
func inputToolDeclarations(raw json.RawMessage) ([]protocol.ResponsesTool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil, nil
	}
	var items []protocol.InputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	var tools []protocol.ResponsesTool
	for _, item := range items {
		if item.Type == "tool_search_output" || item.Type == "additional_tools" {
			tools = append(tools, item.Tools...)
		}
	}
	return tools, nil
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

	outputCallIDs := make(map[string]struct{})
	for _, it := range items {
		if (it.Type == "function_call_output" || it.Type == "custom_tool_call_output" || it.Type == "tool_search_output") && it.CallID != "" {
			outputCallIDs[it.CallID] = struct{}{}
		}
	}

	out := make([]protocol.ChatMessage, 0, len(items))
	// pendingReasoning 暂存上一条 reasoning 输入项的摘要，仅作缓存未命中时的兜底；
	// reasoning 回传优先取网关缓存里的原文（Codex 重放的 reasoning 不可靠）。
	pendingReasoning := ""
	// toolMsg 指向当前正在累积工具调用的 assistant 消息，连续 function_call 合并进同一条。
	var toolMsg *protocol.ChatMessage
	// Chat tool messages cannot carry images. Media extracted from a result batch
	// is emitted as one synthetic user message immediately after all tool outputs.
	var pendingMedia []protocol.ChatContentPart
	// Providers commonly require assistant(tool_calls) and matching tool outputs to be adjacent.
	// Defer intervening messages only when a matching output exists later in this request.
	awaitingToolOutputs := make(map[string]struct{})
	deferredMessages := make([]protocol.ChatMessage, 0)
	flushPendingMedia := func() {
		if len(pendingMedia) == 0 {
			return
		}
		out = append(out, protocol.ChatMessage{Role: "user", ContentParts: pendingMedia})
		pendingMedia = nil
	}
	appendRegularMessage := func(msg protocol.ChatMessage) {
		if len(awaitingToolOutputs) > 0 {
			deferredMessages = append(deferredMessages, msg)
			return
		}
		flushPendingMedia()
		out = append(out, msg)
	}
	flushDeferredMessages := func() {
		flushPendingMedia()
		out = append(out, deferredMessages...)
		deferredMessages = deferredMessages[:0]
	}
	queueToolMedia := func(callID string, media []protocol.ChatContentPart) {
		if len(media) == 0 {
			return
		}
		pendingMedia = append(pendingMedia, protocol.ChatContentPart{
			Type: "text",
			Text: fmt.Sprintf("[apid: media output of tool call %s]", callID),
		})
		pendingMedia = append(pendingMedia, media...)
	}

	for i, it := range items {
		if it.Type != "function_call" && it.Type != "custom_tool_call" && it.Type != "tool_search_call" {
			toolMsg = nil
		}
		switch it.Type {
		case "function_call", "custom_tool_call", "tool_search_call":
			name := it.Name
			callID := it.CallID
			if it.Namespace != "" {
				name = joinToolName(it.Namespace, it.Name)
			}
			arguments := chatToolArguments(it.Arguments)
			if it.Type == "custom_tool_call" {
				arguments = wrapCustomToolInput(it.Input)
			} else if it.Type == "tool_search_call" {
				name = "tool_search"
				if callID == "" {
					callID = it.ID
				}
				if arguments == "" {
					arguments = "{}"
				}
			}
			call := protocol.ChatToolCall{
				ID:   callID,
				Type: "function",
				Function: protocol.ChatToolCallFunction{
					Name:      name,
					Arguments: arguments,
				},
			}
			if toolMsg == nil {
				// 新开一轮并行工具调用，reasoning_content 挂在首条上：
				// 优先取缓存里按 call_id 存的原文，未命中再用 input 摘要兜底。
				rc := lookupCall(src, callID)
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
			if _, ok := outputCallIDs[callID]; ok {
				awaitingToolOutputs[callID] = struct{}{}
			}

		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			content := extractText(it.Output)
			var cleanedOutput json.RawMessage
			if plan := planToolOutputMedia(it.Output); plan != nil {
				content = plan.content
				cleanedOutput = plan.output
				queueToolMedia(it.CallID, plan.media)
			}
			if it.Type == "tool_search_output" {
				content = toolSearchOutputContent(it, cleanedOutput)
			}
			out = append(out, protocol.ChatMessage{
				Role:       "tool",
				ToolCallID: it.CallID,
				Content:    content,
			})
			delete(awaitingToolOutputs, it.CallID)
			if len(awaitingToolOutputs) == 0 {
				flushDeferredMessages()
			}

		case "reasoning":
			pendingReasoning = summaryText(it.Summary)

		case "additional_tools":
			// Declarations were merged into Chat tools before parsing history.

		case "input_text", "input_image":
			role := mapRole(it.Role)
			if role == "" {
				role = "user"
			}
			content, parts := responsesPartsToChat([]protocol.InputContentPart{{
				Type: it.Type, Text: it.Text, ImageURL: it.ImageURL,
				FileID: it.FileID, Detail: it.Detail,
			}})
			appendRegularMessage(protocol.ChatMessage{Role: role, Content: content, ContentParts: parts})

		case "", "message":
			if it.Role == "" {
				return nil, fmt.Errorf("input message at index %d is missing role", i)
			}
			role := mapRole(it.Role)
			content, parts := responsesContentToChat(it.Content)
			msg := protocol.ChatMessage{Role: role, Content: content, ContentParts: parts}
			if role == "assistant" {
				// 按 assistant 文本指纹取回原文，未命中用摘要兜底。
				rc := lookupContent(src, content)
				if rc == "" {
					rc = pendingReasoning
				}
				msg.ReasoningContent = rc
				pendingReasoning = ""
			}
			appendRegularMessage(msg)

		default:
			return nil, fmt.Errorf("unsupported input item type %q at index %d", it.Type, i)
		}
	}

	flushPendingMedia()
	flushDeferredMessages()
	return out, nil
}

func chatToolArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		return compact.String()
	}
	return string(raw)
}

func toolSearchOutputContent(item protocol.InputItem, output json.RawMessage) string {
	b, err := json.Marshal(struct {
		Type      string                   `json:"type"`
		ID        string                   `json:"id,omitempty"`
		CallID    string                   `json:"call_id"`
		Execution string                   `json:"execution,omitempty"`
		Status    string                   `json:"status,omitempty"`
		Tools     []protocol.ResponsesTool `json:"tools"`
		Output    json.RawMessage          `json:"output,omitempty"`
	}{
		Type:      "tool_search_output",
		ID:        item.ID,
		CallID:    item.CallID,
		Execution: item.Execution,
		Status:    item.Status,
		Tools:     item.Tools,
		Output:    output,
	})
	if err != nil {
		return `{"type":"tool_search_output","tools":[]}`
	}
	return string(b)
}

func wrapCustomToolInput(input string) string {
	b, err := json.Marshal(struct {
		Input string `json:"input"`
	}{Input: input})
	if err != nil {
		return `{"input":""}`
	}
	return string(b)
}

func unwrapCustomToolInput(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var wrapped struct {
		Input *string `json:"input"`
	}
	if json.Unmarshal([]byte(arguments), &wrapped) == nil && wrapped.Input != nil {
		return *wrapped.Input
	}
	return arguments
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

// mapRole maps Responses roles to roles accepted by broadly compatible Chat providers.
func mapRole(role string) string {
	if role == "developer" {
		return "user"
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
