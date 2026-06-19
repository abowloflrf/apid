package types

import "encoding/json"

// ---------- Chat Completions API 请求 ----------

// ChatRequest 对应上游 POST /v1/chat/completions 的请求体。
type ChatRequest struct {
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	MaxTokens         *int            `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	Tools             []ChatTool      `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
}

// StreamOptions 对应 Chat Completions 流式请求的 stream_options。
// IncludeUsage 为 true 时，上游会在流末尾多发一个带 usage 的分片
// （choices 为空数组），否则流式默认完全不返回 token 用量。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage 是一条聊天消息。
// 本项目内部 content 统一为纯字符串；assistant 的工具调用放在 ToolCalls，
// 工具结果用 role=="tool" + ToolCallID 表达。
// 上游响应里 content 可能是字符串、内容块数组或缺省(配 refusal)，
// 这些形态由 UnmarshalJSON 归一(见 chat_reasoning.go)。
type ChatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	Refusal          string         `json:"refusal,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

// ChatTool 是请求里声明的一个函数工具。
type ChatTool struct {
	Type     string           `json:"type"` // "function"
	Function ChatToolFunction `json:"function"`
}

type ChatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatToolCall 既用于响应里的完整工具调用，也用于流式分片(带 Index)。
type ChatToolCall struct {
	Index    *int                 `json:"index,omitempty"`
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"`
	Function ChatToolCallFunction `json:"function"`
}

type ChatToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ---------- Chat Completions API 响应（非流式） ----------

// ChatResponse 对应非流式聊天补全响应。
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatUsage struct {
	PromptTokens            int                          `json:"prompt_tokens"`
	CompletionTokens        int                          `json:"completion_tokens"`
	TotalTokens             int                          `json:"total_tokens"`
	PromptTokensDetails     *ChatPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *ChatCompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// ChatPromptTokensDetails 是 usage.prompt_tokens_details，含缓存命中信息。
// 该字段由 OpenAI 官方及部分兼容上游(vLLM 等)返回，上游不给时为 nil。
type ChatPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ChatCompletionTokensDetails 是 usage.completion_tokens_details，含思考 token 数。
// 思考模型上游返回 reasoning_tokens，映射为 Responses 的 output_tokens_details。
type ChatCompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ---------- Chat Completions API 流式分片 ----------

// ChatStreamChunk 对应 stream=true 时每个 SSE data 行的 JSON。
type ChatStreamChunk struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []ChatStreamChoice `json:"choices"`
	Usage   *ChatUsage         `json:"usage,omitempty"`
}

type ChatStreamChoice struct {
	Index        int            `json:"index"`
	Delta        ChatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type ChatChunkDelta struct {
	Role             string         `json:"role,omitempty"`
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}
