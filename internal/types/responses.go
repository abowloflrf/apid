// Package types 定义 Responses API 与 Chat Completions API 的请求 / 响应结构。
// 本 demo 覆盖最关键的字段：文本、工具调用(function calling)、reasoning。
package types

import "encoding/json"

// ---------- Responses API 请求 ----------

// ResponsesRequest 对应 POST /v1/responses 的请求体。
// input / tool_choice 在官方协议里是多形态联合类型，用 json.RawMessage 延迟解析。
type ResponsesRequest struct {
	Model             string           `json:"model"`
	Input             json.RawMessage  `json:"input"`
	Instructions      string           `json:"instructions,omitempty"`
	Temperature       *float64         `json:"temperature,omitempty"`
	TopP              *float64         `json:"top_p,omitempty"`
	MaxOutputTokens   *int             `json:"max_output_tokens,omitempty"`
	Stream            bool             `json:"stream,omitempty"`
	Tools             []ResponsesTool  `json:"tools,omitempty"`
	ToolChoice        json.RawMessage  `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	Reasoning         *ReasoningConfig `json:"reasoning,omitempty"`
}

// ResponsesTool 对应 tools 数组里的一项。
// 处理自定义函数工具(type=="function")；服务端内置工具(web_search 等)会被忽略。
// type=="namespace"(如 MCP 工具组)时，真正的函数工具嵌在 Tools 里，转换时展开。
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
	// namespace 工具组(MCP)内嵌的子工具，每个通常是 type=="function"。
	Tools []ResponsesTool `json:"tools,omitempty"`
}

// ReasoningConfig 对应请求里的 reasoning 配置。
type ReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// InputItem 是 input 数组里的一项。它可能是：
//   - 一条消息(type 缺省或 "message")：使用 Role + Content
//   - 一次函数调用(type=="function_call")：使用 CallID/Name/Arguments
//   - 一个函数调用结果(type=="function_call_output")：使用 CallID/Output
//   - 一段 reasoning(type=="reasoning")：demo 中转发到上游时会忽略
type InputItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call 的命名空间(MCP 工具组)。多轮回传时与本地 Name 一起拼回上游的
	// 扁平全限定名，使其与发给上游的工具定义一致。
	Namespace string `json:"namespace,omitempty"`
	// function_call_output：可能是字符串或内容块数组
	Output json.RawMessage `json:"output,omitempty"`
	// reasoning：思考模型的推理摘要，多轮时需回传给上游
	Summary []SummaryText `json:"summary,omitempty"`
}

// InputContentPart 对应内容块数组里的一个元素，如 {"type":"input_text","text":"..."}。
type InputContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ---------- Responses API 响应（非流式） ----------

// ResponsesResponse 对应非流式响应的顶层对象。
type ResponsesResponse struct {
	ID        string `json:"id"`
	Object    string `json:"object"` // 固定为 "response"
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"` // "completed" / "incomplete" / "failed"
	// IncompleteDetails 仅在 Status=="incomplete" 时出现，说明被截断的原因
	// （由上游 finish_reason 映射而来，见 convert.mapFinishReason）。
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Model             string             `json:"model"`
	Output            []OutputItem       `json:"output"`
	Usage             *ResponseUsage     `json:"usage,omitempty"`
}

// IncompleteDetails 对应 Responses 响应的 incomplete_details 字段。
// reason 取值如 "max_output_tokens"(对应上游 finish_reason="length")、
// "content_filter"(对应上游 finish_reason="content_filter")。
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// OutputItem 是 output 数组里的一项，可能是 message / function_call / reasoning。
type OutputItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	// message
	Role    string          `json:"role,omitempty"`
	Content []OutputContent `json:"content,omitempty"`
	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call 的命名空间(MCP 工具组)。Codex 等客户端按 {name, namespace}
	// 在注册表里精确匹配工具，缺这个字段会路由失败(unsupported call)。
	Namespace string `json:"namespace,omitempty"`
	// reasoning
	Summary []SummaryText `json:"summary,omitempty"`
}

// OutputContent 是 message 内容块：output_text(正常文本)或 refusal(模型拒绝)。
// 两种形态字段不同——output_text 是 {type,text,annotations}，refusal 按规范
// 只有 {type,refusal}——故用 MarshalJSON 各自只输出规范要求的字段。
type OutputContent struct {
	Type        string `json:"type"` // "output_text" / "refusal"
	Text        string `json:"text"`
	Refusal     string `json:"refusal"`
	Annotations []any  `json:"annotations"`
}

func (c OutputContent) MarshalJSON() ([]byte, error) {
	if c.Type == "refusal" {
		return json.Marshal(struct {
			Type    string `json:"type"`
			Refusal string `json:"refusal"`
		}{c.Type, c.Refusal})
	}
	ann := c.Annotations
	if ann == nil {
		ann = []any{}
	}
	return json.Marshal(struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}{c.Type, c.Text, ann})
}

// SummaryText 是 reasoning 输出项里的摘要块。
type SummaryText struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

// ResponseUsage 对应 Responses API 的 usage 字段。
type ResponseUsage struct {
	InputTokens         int                  `json:"input_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                  `json:"output_tokens"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                  `json:"total_tokens"`
}

// InputTokensDetails 是 Responses usage.input_tokens_details，含缓存命中信息。
// 由上游 Chat 的 prompt_tokens_details 映射而来。
type InputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// OutputTokensDetails 是 Responses usage.output_tokens_details，含思考 token 数。
// 由上游 Chat 的 completion_tokens_details 映射而来。
type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}
