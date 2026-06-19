package types

import (
	"encoding/json"
	"testing"
)

func TestChatMessageReasoningExtraction(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"reasoning_content", `{"role":"assistant","reasoning_content":"想一下"}`, "想一下"},
		{"reasoning string", `{"role":"assistant","reasoning":"思考中"}`, "思考中"},
		{"reasoning object content", `{"reasoning":{"content":"对象content"}}`, "对象content"},
		{"reasoning object text", `{"reasoning":{"text":"对象text"}}`, "对象text"},
		{"reasoning object summary", `{"reasoning":{"summary":"对象summary"}}`, "对象summary"},
		{"reasoning_details string", `{"reasoning_details":"细节字符串"}`, "细节字符串"},
		{"reasoning_details array", `{"reasoning_details":[{"text":"a"},{"text":"b"}]}`, "a\n\nb"},
		{"reasoning_details object", `{"reasoning_details":{"content":"单块"}}`, "单块"},
		{"reasoning_details nested parts", `{"reasoning_details":[{"parts":[{"text":"x"},{"text":"y"}]}]}`, "x\n\ny"},
		{"priority reasoning_content first", `{"reasoning_content":"主","reasoning":"次"}`, "主"},
		{"empty reasoning falls back to content", `{"reasoning":"","reasoning_content":"兜底"}`, "兜底"},
		{"none", `{"role":"assistant","content":"hi"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m ChatMessage
			if err := json.Unmarshal([]byte(c.raw), &m); err != nil {
				t.Fatal(err)
			}
			if m.ReasoningContent != c.want {
				t.Errorf("got %q, want %q", m.ReasoningContent, c.want)
			}
		})
	}
}

// 多态 reasoning 解析不得破坏 message 的其它字段。
func TestChatMessageFieldsPreserved(t *testing.T) {
	raw := `{"role":"assistant","content":"答案","reasoning":"思",
		"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`
	var m ChatMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != "assistant" || m.Content != "答案" || m.ReasoningContent != "思" ||
		len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call_1" {
		t.Errorf("字段未保留: %+v", m)
	}
}

// 流式 delta 同样支持多态 reasoning，且不破坏 content/tool_calls。
func TestChatChunkDeltaReasoningExtraction(t *testing.T) {
	var d ChatChunkDelta
	if err := json.Unmarshal([]byte(`{"reasoning":"流式思考"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.ReasoningContent != "流式思考" {
		t.Errorf("reasoning got %q", d.ReasoningContent)
	}

	var d2 ChatChunkDelta
	raw := `{"content":"hi","tool_calls":[{"index":0,"id":"c1","function":{"name":"f"}}]}`
	if err := json.Unmarshal([]byte(raw), &d2); err != nil {
		t.Fatal(err)
	}
	if d2.Content != "hi" || len(d2.ToolCalls) != 1 || d2.ToolCalls[0].ID != "c1" {
		t.Errorf("delta 字段被破坏: %+v", d2)
	}
}
