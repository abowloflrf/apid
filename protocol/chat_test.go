package protocol

import (
	"encoding/json"
	"testing"
)

func TestChatMessageMarshalContent(t *testing.T) {
	calls := []ChatToolCall{{ID: "call_1", Type: "function", Function: ChatToolCallFunction{Name: "lookup", Arguments: "{}"}}}
	tests := []struct {
		name string
		msg  ChatMessage
		want string
	}{
		{"tool only", ChatMessage{Role: "assistant", ToolCalls: calls}, `null`},
		{"reasoning and tool", ChatMessage{Role: "assistant", ReasoningContent: "thinking", ToolCalls: calls}, `null`},
		{"text and tool", ChatMessage{Role: "assistant", Content: "Checking.", ToolCalls: calls}, `"Checking."`},
		{"empty tool result", ChatMessage{Role: "tool", ToolCallID: "call_1"}, `""`},
		{"empty assistant", ChatMessage{Role: "assistant"}, `""`},
		{"empty user", ChatMessage{Role: "user"}, `""`},
		{"image", ChatMessage{Role: "user", ContentParts: []ChatContentPart{{Type: "image_url", ImageURL: &ChatImageURL{URL: "https://example.com/image.png"}}}}, `[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if got := string(fields["content"]); got != tt.want {
				t.Fatalf("content = %s, want %s", got, tt.want)
			}
			if len(tt.msg.ToolCalls) > 0 {
				wantCalls, err := json.Marshal(tt.msg.ToolCalls)
				if err != nil {
					t.Fatal(err)
				}
				if string(fields["tool_calls"]) != string(wantCalls) {
					t.Fatalf("tool_calls changed: %s", fields["tool_calls"])
				}
			}
			if tt.msg.ReasoningContent != "" && string(fields["reasoning_content"]) != `"thinking"` {
				t.Fatalf("reasoning_content changed: %s", fields["reasoning_content"])
			}
		})
	}
}
