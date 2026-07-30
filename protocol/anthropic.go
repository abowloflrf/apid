package protocol

// AnthropicUsage is the usage object returned by Anthropic Messages responses.
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// AnthropicStreamEvent is the minimal SSE event shape needed for stats.
type AnthropicStreamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"` // input_json_delta: streamed tool-use args
	} `json:"delta"`
	Message struct {
		Usage *AnthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *AnthropicUsage `json:"usage"`
}
