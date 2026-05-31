// Package convert 实现 Responses API 与 Chat Completions API 之间的字段转换。
package convert

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/abowloflrf/apid/internal/types"
)

// ResponsesToChat 把一个 Responses API 请求转换为 Chat Completions 请求。
// 同时返回「扁平工具名 -> (命名空间, 本地名)」映射(见 expandTools)，供响应方向把
// MCP 工具的 function_call 从上游扁平名拆回本地名 + namespace。
func ResponsesToChat(r *types.ResponsesRequest) (*types.ChatRequest, map[string]NamespacedTool, error) {
	messages := make([]types.ChatMessage, 0, 4)

	// instructions 映射为 system 消息，放在最前面。
	if r.Instructions != "" {
		messages = append(messages, types.ChatMessage{Role: "system", Content: r.Instructions})
	}

	inputMsgs, err := parseInput(r.Input)
	if err != nil {
		return nil, nil, err
	}
	messages = append(messages, inputMsgs...)

	tools, namespaces := expandTools(r.Tools)
	chat := &types.ChatRequest{
		Model:             r.Model,
		Messages:          messages,
		Temperature:       r.Temperature,
		TopP:              r.TopP,
		MaxTokens:         r.MaxOutputTokens, // max_output_tokens -> max_tokens
		Stream:            r.Stream,
		Tools:             tools,
		ToolChoice:        convertToolChoice(r.ToolChoice),
		ParallelToolCalls: r.ParallelToolCalls, // 透传，控制并行工具调用
	}

	// 流式时强制注入 stream_options.include_usage，
	// 否则上游 Chat Completions 流式默认不返回 token 用量，
	// 转出去的 response.completed 也就拿不到 usage。
	if chat.Stream {
		chat.StreamOptions = &types.StreamOptions{IncludeUsage: true}
	}

	// reasoning.effort -> reasoning_effort
	if r.Reasoning != nil && r.Reasoning.Effort != "" {
		chat.ReasoningEffort = r.Reasoning.Effort
	}

	return chat, namespaces, nil
}

// NamespacedTool 记录一个命名空间工具拆解后的两部分。
// Codex 这类客户端用 ToolName{name, namespace} 在注册表里精确匹配，其中
// name 是**本地名**(如 tavily_search)、namespace 是命名空间(如 mcp__tavily)。
type NamespacedTool struct {
	Namespace string // 命名空间，如 mcp__tavily
	Name      string // 本地工具名，如 tavily_search
}

// expandTools 递归展开 Responses 工具定义，一次遍历得到两样东西：
//   - 发给上游的扁平 Chat 工具列表；namespace 工具组(MCP)展开为扁平名
//     "<namespace>__<tool>"，使发给上游的工具名唯一。
//   - 「扁平名 -> (命名空间, 本地名)」映射；响应方向据此把扁平名拆回本地名 +
//     namespace，否则 Codex 注册表按 {name, namespace} 匹配不上(unsupported call)。
//
// 服务端内置工具(web_search 等)无法在 Chat Completions 表达，跳过并记录日志。
func expandTools(tools []types.ResponsesTool) ([]types.ChatTool, map[string]NamespacedTool) {
	var chat []types.ChatTool
	namespaces := make(map[string]NamespacedTool)

	var walk func(tools []types.ResponsesTool, prefix string)
	walk = func(tools []types.ResponsesTool, prefix string) {
		for _, t := range tools {
			switch {
			case t.Type == "namespace":
				// 命名空间本身不可调用，下钻其子工具，命名空间名累加进前缀。
				walk(t.Tools, joinToolName(prefix, t.Name))
			case t.Type == "function" && t.Name != "":
				flat := joinToolName(prefix, t.Name)
				chat = append(chat, types.ChatTool{
					Type: "function",
					Function: types.ChatToolFunction{
						Name:        flat,
						Description: t.Description,
						Parameters:  ensureObjectSchema(t.Parameters),
						Strict:      t.Strict,
					},
				})
				if prefix != "" { // 仅命名空间下的工具需要拆分还原
					namespaces[flat] = NamespacedTool{Namespace: prefix, Name: t.Name}
				}
			default:
				log.Printf("skipping unsupported tool type: %q", t.Type)
			}
		}
	}
	walk(tools, "")

	if len(chat) == 0 {
		chat = nil // 让 ChatRequest.Tools 的 omitempty 生效
	}
	return chat, namespaces
}

// joinToolName 用 "__" 拼接命名空间前缀与工具名，前缀为空时返回原名。
func joinToolName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "__" + name
}

// ensureObjectSchema 保证 function 工具的 parameters 是带 "type":"object" 的 JSON Schema。
// Responses 客户端可能完全不带 parameters、或给一个缺 type 的对象；而 Anthropic 兼容层、
// 部分网关会拒绝这种 function 工具。缺失/空对象时补成 {"type":"object"}，已带 type 则原样透传。
func ensureObjectSchema(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	// 非对象(null / 空 / 数组等)一律兜底为 {"type":"object"}。
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	if _, ok := m["type"]; ok {
		return raw // 已声明 type，尊重客户端原值。
	}
	m["type"] = json.RawMessage(`"object"`)
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return b
}

// convertToolChoice 转换 tool_choice，归一到 Chat Completions 认识的形态：
// 字符串("auto"/"none"/"required")直接透传；具名函数 {"type":"function","name":"x"}
// 转成 Chat 的嵌套形式 {"type":"function","function":{"name":"x"}}。
//
// 此外把 Cursor 等客户端发的"无函数名对象形式"降级为等价字符串，否则上游不认：
//
//	{"type":"auto"}            -> "auto"
//	{"type":"none"}            -> "none"
//	{"type":"required"|"tool"|"any"} -> "required"   ("tool" 表示"必须用某个工具但不指定哪个")
//	{"type":"function"} 缺 name      -> "required"
//
// 无法识别的形态原样透传。
func convertToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw // 字符串形式，原样透传
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return raw // 非对象，无法识别
	}
	// 具名函数：转成 Chat 的嵌套 function 形式。
	if obj.Type == "function" && obj.Name != "" {
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": obj.Name},
		})
		return b
	}
	// 无函数名的对象形式，降级为等价字符串。
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

// parseInput 解析 Responses 的 input 字段，它可能是：
//  1. 一个字符串（等价于一条 user 消息）
//  2. 一个 input 项数组（消息 / function_call / function_call_output / reasoning）
func parseInput(raw json.RawMessage) ([]types.ChatMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	// 情况 1：纯字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []types.ChatMessage{{Role: "user", Content: s}}, nil
	}

	// 情况 2：input 项数组
	var items []types.InputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("无法解析 input 字段: %w", err)
	}

	out := make([]types.ChatMessage, 0, len(items))
	// pendingReasoning 暂存上一条 reasoning 项的文本，待挂到紧随其后的
	// assistant 消息(function_call / message)上。思考模型上游要求多轮时
	// 把 assistant 的 reasoning_content 一并回传，否则会报
	// "reasoning_content in the thinking mode must be passed back"。
	pendingReasoning := ""
	// toolMsg 指向当前正在累积工具调用的 assistant 消息。连续的 function_call
	// 项(并行工具调用)必须合并进同一条 assistant.tool_calls，否则上游会报
	// "assistant message with 'tool_calls' must be followed by tool messages"。
	// 遇到任何非 function_call 项时清空，结束这一轮累积。
	var toolMsg *types.ChatMessage
	for _, it := range items {
		if it.Type != "function_call" {
			toolMsg = nil
		}
		switch it.Type {
		case "function_call":
			// 带 namespace 的(MCP 工具组)要拼回上游的扁平全限定名，
			// 与发给上游的工具定义一致；否则上游会拒认这个工具调用。
			name := it.Name
			if it.Namespace != "" {
				name = joinToolName(it.Namespace, it.Name)
			}
			call := types.ChatToolCall{
				ID:   it.CallID,
				Type: "function",
				Function: types.ChatToolCallFunction{
					Name:      name,
					Arguments: it.Arguments,
				},
			}
			if toolMsg == nil {
				// 开启一轮并行工具调用，reasoning_content 挂在首条上。
				out = append(out, types.ChatMessage{
					Role:             "assistant",
					ReasoningContent: pendingReasoning,
				})
				pendingReasoning = ""
				toolMsg = &out[len(out)-1]
			}
			toolMsg.ToolCalls = append(toolMsg.ToolCalls, call)
		case "function_call_output":
			// 工具执行结果。
			out = append(out, types.ChatMessage{
				Role:       "tool",
				ToolCallID: it.CallID,
				Content:    extractText(it.Output),
			})
		case "reasoning":
			// 暂存推理文本，挂到下一条 assistant 消息上回传给上游。
			pendingReasoning = summaryText(it.Summary)
		default: // "" 或 "message"
			role := mapRole(it.Role)
			msg := types.ChatMessage{Role: role, Content: extractText(it.Content)}
			if role == "assistant" {
				msg.ReasoningContent = pendingReasoning
				pendingReasoning = ""
			}
			out = append(out, msg)
		}
	}
	return out, nil
}

// summaryText 把 reasoning 项的 summary 块拼接成纯文本。
func summaryText(summary []types.SummaryText) string {
	var b strings.Builder
	for _, s := range summary {
		b.WriteString(s.Text)
	}
	return b.String()
}

// mapRole 把 Responses 消息角色映射为 Chat Completions 支持的角色。
// Responses API 用 "developer" 表示开发者指令，Chat Completions 不认，归一为 "system"。
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

	// 情况 1：纯字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// 情况 2：内容块数组，拼接所有文本块。
	var parts []types.InputContentPart
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
