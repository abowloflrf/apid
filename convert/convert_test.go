package convert

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/abowloflrf/apid/protocol"
)

// sink 实现 SSEWriter，收集所有写入内容用于断言。
type sink struct{ b strings.Builder }

func (s *sink) Write(p []byte) (int, error) { return s.b.Write(p) }
func (s *sink) Flush()                      {}

type failingSink struct{ err error }

func (s failingSink) Write([]byte) (int, error) { return 0, s.err }
func (s failingSink) Flush()                    {}

func parseSSEPayloads(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatalf("decode SSE payload: %v", err)
		}
		payloads = append(payloads, payload)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE payloads: %v", err)
	}
	return payloads
}

func TestRequestTools(t *testing.T) {
	body := `{
      "model":"m",
      "input":"今天天气如何",
      "tools":[{"type":"function","name":"get_weather","description":"查天气","parameters":{"type":"object"}}],
      "tool_choice":{"type":"function","name":"get_weather"},
      "reasoning":{"effort":"high"}
    }`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("工具未正确转换: %+v", chat.Tools)
	}
	if chat.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q", chat.ReasoningEffort)
	}
	// tool_choice 应转成嵌套 function 形式
	if !strings.Contains(string(chat.ToolChoice), `"function":{"name":"get_weather"}`) {
		t.Errorf("tool_choice 转换错误: %s", chat.ToolChoice)
	}
}

func TestRequestToolSearchBuildsDynamicToolContext(t *testing.T) {
	body := `{
      "model":"m",
      "input":[
        {"role":"user","content":"find a filesystem tool"},
        {"type":"tool_search_call","id":"call_search","execution":"client","arguments":{"query":"filesystem","limit":3}},
        {"type":"tool_search_output","id":"tso_1","call_id":"call_search","execution":"client","tools":[
          {"type":"function","name":"shared","description":"dynamic duplicate","parameters":{"type":"object"}},
          {"type":"namespace","name":"fs","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]},
          {"type":"custom","name":"apply_patch"}
        ]},
        {"type":"additional_tools","tools":[
          {"type":"function","name":"extra","parameters":{"properties":{"value":{"type":"string"}}}}
        ]}
      ],
      "tools":[
        {"type":"tool_search","execution":"client"},
        {"type":"function","name":"shared","description":"top-level wins","parameters":{"type":"object"}}
      ],
      "tool_choice":{"type":"tool_search"}
    }`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, tools, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}

	wantNames := []string{"tool_search", "shared", "fs__read_file", "apply_patch", "extra"}
	if len(chat.Tools) != len(wantNames) {
		t.Fatalf("tool count = %d, want %d: %+v", len(chat.Tools), len(wantNames), chat.Tools)
	}
	for i, want := range wantNames {
		if got := chat.Tools[i].Function.Name; got != want {
			t.Fatalf("tool[%d] name = %q, want %q", i, got, want)
		}
	}
	if got := chat.Tools[1].Function.Description; got != "top-level wins" {
		t.Fatalf("duplicate tool precedence = %q", got)
	}
	if ref := tools["tool_search"]; ref.Kind != "tool_search" || ref.Execution != "client" {
		t.Fatalf("tool_search context = %+v", ref)
	}
	if ref := tools["fs__read_file"]; ref.Kind != "function" || ref.Namespace != "fs" || ref.Name != "read_file" {
		t.Fatalf("dynamic namespace context = %+v", ref)
	}
	if ref := tools["apply_patch"]; ref.Kind != "custom" || ref.Name != "apply_patch" {
		t.Fatalf("dynamic custom context = %+v", ref)
	}
	if !strings.Contains(string(chat.ToolChoice), `"function":{"name":"tool_search"}`) {
		t.Fatalf("tool_search choice = %s", chat.ToolChoice)
	}

	if len(chat.Messages) != 3 {
		t.Fatalf("message count = %d, want 3: %+v", len(chat.Messages), chat.Messages)
	}
	call := chat.Messages[1].ToolCalls[0]
	if call.ID != "call_search" || call.Function.Name != "tool_search" || call.Function.Arguments != `{"query":"filesystem","limit":3}` {
		t.Fatalf("tool_search call = %+v", call)
	}
	result := chat.Messages[2]
	if result.Role != "tool" || result.ToolCallID != "call_search" {
		t.Fatalf("tool_search output message = %+v", result)
	}
	var output struct {
		Type  string                   `json:"type"`
		Tools []protocol.ResponsesTool `json:"tools"`
	}
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("decode tool_search output: %v; content=%s", err, result.Content)
	}
	if output.Type != "tool_search_output" || len(output.Tools) != 3 {
		t.Fatalf("tool_search output content = %+v", output)
	}
}

func TestDynamicToolContextRestoresLoadedTool(t *testing.T) {
	body := `{
      "model":"m",
      "input":[{"type":"additional_tools","tools":[
        {"type":"namespace","name":"db","tools":[{"type":"function","name":"query","parameters":{"type":"object"}}]}
      ]}]
    }`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chatReq, tools, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chatReq.Messages) != 0 || len(chatReq.Tools) != 1 || chatReq.Tools[0].Function.Name != "db__query" {
		t.Fatalf("additional_tools conversion = %+v", chatReq)
	}

	chatResp := &protocol.ChatResponse{
		ID: "chatcmpl-dynamic", Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{Role: "assistant", ToolCalls: []protocol.ChatToolCall{{
				ID: "call_query", Function: protocol.ChatToolCallFunction{Name: "db__query", Arguments: `{"sql":"select 1"}`},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	call := ChatToResponses(chatResp, tools).Output[0]
	if call.Type != "function_call" || call.Name != "query" || call.Namespace != "db" {
		t.Fatalf("restored dynamic tool call = %+v", call)
	}
}

func TestRequestCustomTool(t *testing.T) {
	body := `{
      "model":"m",
      "input":[
        {"role":"user","content":"apply this patch"},
        {"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n+line with \"quotes\"\n*** End Patch"},
        {"type":"custom_tool_call_output","call_id":"call_1","output":"Done"}
      ],
      "tools":[{
        "type":"custom",
        "name":"apply_patch",
        "description":"Apply a patch",
        "format":{"type":"grammar","syntax":"lark","definition":"root: /.+/"}
      }],
      "tool_choice":{"type":"custom","name":"apply_patch"}
    }`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, tools, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 1 {
		t.Fatalf("custom tool count = %d, want 1", len(chat.Tools))
	}
	tool := chat.Tools[0]
	if tool.Type != "function" || tool.Function.Name != "apply_patch" ||
		!strings.Contains(tool.Function.Description, "Apply a patch") ||
		!strings.Contains(tool.Function.Description, `"type":"grammar"`) {
		t.Fatalf("custom tool was not converted to a Chat function: %+v", tool)
	}
	var schema struct {
		Type       string `json:"type"`
		Properties struct {
			Input struct {
				Type string `json:"type"`
			} `json:"input"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.Properties.Input.Type != "string" ||
		len(schema.Required) != 1 || schema.Required[0] != "input" {
		t.Fatalf("custom tool schema = %+v", schema)
	}
	if ref := tools["apply_patch"]; ref.Kind != "custom" || ref.Name != "apply_patch" {
		t.Fatalf("custom tool mapping = %+v", ref)
	}
	if !strings.Contains(string(chat.ToolChoice), `"function":{"name":"apply_patch"}`) {
		t.Fatalf("custom tool_choice = %s", chat.ToolChoice)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("message count = %d, want 3: %+v", len(chat.Messages), chat.Messages)
	}
	call := chat.Messages[1].ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "apply_patch" {
		t.Fatalf("custom call identity = %+v", call)
	}
	wantInput := "*** Begin Patch\n+line with \"quotes\"\n*** End Patch"
	if got := unwrapCustomToolInput(call.Function.Arguments); got != wantInput {
		t.Fatalf("custom call input = %q, want %q; arguments=%s", got, wantInput, call.Function.Arguments)
	}
	result := chat.Messages[2]
	if result.Role != "tool" || result.ToolCallID != "call_1" || result.Content != "Done" {
		t.Fatalf("custom tool output = %+v", result)
	}
}

func TestNamespacedCustomToolRoundTrip(t *testing.T) {
	body := `{
      "model":"m",
      "input":[{"type":"custom_tool_call","call_id":"call_1","namespace":"shell","name":"exec","input":"pwd"}],
      "tools":[{"type":"namespace","name":"shell","tools":[{"type":"custom","name":"exec"}]}],
      "tool_choice":{"type":"custom","namespace":"shell","name":"exec"}
    }`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, tools, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "shell__exec" {
		t.Fatalf("namespaced custom tool = %+v", chat.Tools)
	}
	if !strings.Contains(string(chat.ToolChoice), `"function":{"name":"shell__exec"}`) {
		t.Fatalf("namespaced custom tool_choice = %s", chat.ToolChoice)
	}
	if got := chat.Messages[0].ToolCalls[0].Function.Name; got != "shell__exec" {
		t.Fatalf("namespaced custom call name = %q", got)
	}
	ref := tools["shell__exec"]
	if ref.Kind != "custom" || ref.Namespace != "shell" || ref.Name != "exec" {
		t.Fatalf("namespaced custom mapping = %+v", ref)
	}

	chatResp := &protocol.ChatResponse{
		ID: "chatcmpl-x", Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{Role: "assistant", ToolCalls: []protocol.ChatToolCall{{
				ID:       "call_2",
				Function: protocol.ChatToolCallFunction{Name: "shell__exec", Arguments: `{"input":"ls"}`},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	call := ChatToResponses(chatResp, tools).Output[0]
	if call.Type != "custom_tool_call" || call.Name != "exec" || call.Namespace != "shell" ||
		call.Input == nil || *call.Input != "ls" {
		t.Fatalf("restored namespaced custom call = %+v", call)
	}
}

func TestRequestRejectsUnsupportedInputItem(t *testing.T) {
	body := `{"model":"m","input":[{"type":"web_search_call","id":"ws_1"}]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResponsesToChat(&req, nil)
	if err == nil {
		t.Fatal("unsupported input item should fail instead of producing an empty Chat message")
	}
	if !strings.Contains(err.Error(), `unsupported input item type "web_search_call" at index 0`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequestRejectsMessageWithoutRole(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","content":"orphan"}]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResponsesToChat(&req, nil)
	if err == nil || !strings.Contains(err.Error(), "missing role") {
		t.Fatalf("message without role error = %v", err)
	}
}

func TestRequestResponseFormatJSONSchema(t *testing.T) {
	body := `{
      "model":"m",
      "input":"review",
      "text":{"format":{
        "type":"json_schema",
        "name":"codex_output_schema",
        "description":"approval result",
        "strict":true,
        "schema":{
          "type":"object",
          "additionalProperties":false,
          "properties":{"outcome":{"type":"string","enum":["allow","deny"]}},
          "required":["outcome"]
        }
      }}
    }`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(chat.ResponseFormat)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"json_schema","json_schema":{"name":"codex_output_schema","description":"approval result","schema":{"type":"object","additionalProperties":false,"properties":{"outcome":{"type":"string","enum":["allow","deny"]}},"required":["outcome"]},"strict":true}}`
	if string(got) != want {
		t.Errorf("response_format = %s, want %s", got, want)
	}
}

func TestRequestResponseFormatSimpleTypes(t *testing.T) {
	for _, formatType := range []string{"text", "json_object"} {
		body := `{"model":"m","input":"hi","text":{"format":{"type":"` + formatType + `"}}}`
		var req protocol.ResponsesRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("%s: %v", formatType, err)
		}
		chat, _, err := ResponsesToChat(&req, nil)
		if err != nil {
			t.Fatalf("%s: %v", formatType, err)
		}
		got, err := json.Marshal(chat.ResponseFormat)
		if err != nil {
			t.Fatalf("%s: %v", formatType, err)
		}
		want := `{"type":"` + formatType + `"}`
		if string(got) != want {
			t.Errorf("%s: response_format = %s, want %s", formatType, got, want)
		}
	}
}

func TestRequestToolParametersDefault(t *testing.T) {
	// parameters 缺失 / 缺 type / 已带 type 三种情形，发给上游时都应是带 type:object 的 schema。
	body := `{"model":"m","input":"hi","tools":[
      {"type":"function","name":"no_params"},
      {"type":"function","name":"empty_params","parameters":{}},
      {"type":"function","name":"with_props","parameters":{"properties":{"x":{"type":"string"}}}},
      {"type":"function","name":"already_typed","parameters":{"type":"array"}}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 4 {
		t.Fatalf("工具数 = %d, 期望 4", len(chat.Tools))
	}
	// 前三个应补出 type:object；末一个已声明 type:array，应原样保留。
	for i, want := range []string{`"type":"object"`, `"type":"object"`, `"type":"object"`, `"type":"array"`} {
		got := string(chat.Tools[i].Function.Parameters)
		if !strings.Contains(got, want) {
			t.Errorf("工具[%d] parameters=%s, 期望含 %s", i, got, want)
		}
	}
	// with_props 应保留原有 properties，仅补 type。
	if !strings.Contains(string(chat.Tools[2].Function.Parameters), `"properties"`) {
		t.Errorf("with_props 丢失了 properties: %s", chat.Tools[2].Function.Parameters)
	}
}

func TestRequestToolChoiceObjectForms(t *testing.T) {
	cases := map[string]string{
		`{"type":"auto"}`:                    `"auto"`,
		`{"type":"none"}`:                    `"none"`,
		`{"type":"tool"}`:                    `"required"`,
		`{"type":"required"}`:                `"required"`,
		`{"type":"any"}`:                     `"required"`,
		`{"type":"function"}`:                `"required"`, // 缺 name 降级
		`"auto"`:                             `"auto"`,     // 字符串原样透传
		`{"type":"function","name":"get_w"}`: `"function":{"name":"get_w"}`,
	}
	for in, want := range cases {
		body := `{"model":"m","input":"hi","tools":[{"type":"function","name":"get_w"}],"tool_choice":` + in + `}`
		var req protocol.ResponsesRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("input %s: %v", in, err)
		}
		chat, _, err := ResponsesToChat(&req, nil)
		if err != nil {
			t.Fatalf("input %s: %v", in, err)
		}
		if got := string(chat.ToolChoice); !strings.Contains(got, want) {
			t.Errorf("tool_choice %s -> %s, 期望含 %s", in, got, want)
		}
	}
}

func TestRequestParallelToolCalls(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[{"type":"function","name":"get_w"}],"parallel_tool_calls":false}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if chat.ParallelToolCalls == nil || *chat.ParallelToolCalls != false {
		t.Errorf("parallel_tool_calls 未透传: %+v", chat.ParallelToolCalls)
	}

	// 未提供时不应注入该字段（保持 nil，序列化时省略）。
	body2 := `{"model":"m","input":"hi"}`
	var req2 protocol.ResponsesRequest
	_ = json.Unmarshal([]byte(body2), &req2)
	chat2, _, _ := ResponsesToChat(&req2, nil)
	if chat2.ParallelToolCalls != nil {
		t.Errorf("未提供时不应设置 parallel_tool_calls, 实际 %+v", chat2.ParallelToolCalls)
	}
}

func TestRequestOmitsToolSettingsWithoutConvertedTools(t *testing.T) {
	cases := map[string]string{
		"empty tools":         `{"model":"m","input":"hi","tools":[],"tool_choice":"auto","parallel_tool_calls":false}`,
		"unconvertible tools": `{"model":"m","input":"hi","tools":[{"type":"web_search"}],"tool_choice":"auto","parallel_tool_calls":false}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var req protocol.ResponsesRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatal(err)
			}
			chat, _, err := ResponsesToChat(&req, nil)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(chat)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
				if _, ok := fields[field]; ok {
					t.Errorf("%s should be omitted without converted tools: %s", field, wire)
				}
			}
		})
	}
}

func TestRequestNamespaceTools(t *testing.T) {
	// MCP 工具(如 Tavily)挂在 type:"namespace" 下，需展开内层 function。
	// 同时混入一个服务端内置工具(web_search)，应被跳过。
	body := `{
      "model":"m",
      "input":"搜一下",
      "tools":[
        {"type":"web_search"},
        {"type":"namespace","name":"mcp__tavily","tools":[
          {"type":"function","name":"tavily_crawl","description":"爬网","parameters":{"type":"object"}},
          {"type":"function","name":"tavily_search","parameters":{"type":"object"}}
        ]}
      ]
    }`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Tools) != 2 {
		t.Fatalf("期望展开出 2 个函数工具, 实际 %d: %+v", len(chat.Tools), chat.Tools)
	}
	// 子工具名需带命名空间前缀，与 Codex 注册的全限定名一致。
	if chat.Tools[0].Function.Name != "mcp__tavily__tavily_crawl" ||
		chat.Tools[1].Function.Name != "mcp__tavily__tavily_search" {
		t.Errorf("namespace 工具未按全限定名展开: %+v", chat.Tools)
	}
}

func TestRequestToolConversation(t *testing.T) {
	// 多轮：assistant 发起调用 + 工具返回结果。
	body := `{"model":"m","input":[
      {"role":"user","content":"天气?"},
      {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
      {"type":"function_call_output","call_id":"call_1","output":"晴 25度"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("消息数 = %d, 期望 3: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[1].Role != "assistant" || len(chat.Messages[1].ToolCalls) != 1 {
		t.Errorf("function_call 未转成 assistant.tool_calls: %+v", chat.Messages[1])
	}
	if chat.Messages[1].ToolCalls[0].ID != "call_1" {
		t.Errorf("call_id 未映射为 tool_call.id")
	}
	wire, err := json.Marshal(chat.Messages[1])
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(wire, &message); err != nil {
		t.Fatal(err)
	}
	content, ok := message["content"]
	if !ok {
		t.Fatal("assistant tool-call message 缺少 content 字段")
	}
	if string(content) != `""` {
		t.Fatalf("assistant tool-call message content = %s, 期望空字符串", content)
	}
	tool := chat.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call_1" || tool.Content != "晴 25度" {
		t.Errorf("function_call_output 未转成 tool 消息: %+v", tool)
	}
}

func TestRequestNamespaceToolConversation(t *testing.T) {
	// 回程：namespace 工具的调用结果带全限定名 mcp__tavily__tavily_search，
	// parseInput 应原样透传该名字，首尾一致。
	body := `{"model":"m","input":[
      {"role":"user","content":"搜一下"},
      {"type":"function_call","call_id":"call_9","name":"mcp__tavily__tavily_search","arguments":"{\"query\":\"go\"}"},
      {"type":"function_call_output","call_id":"call_9","output":"结果"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 {
		t.Fatalf("消息数 = %d, 期望 3: %+v", len(chat.Messages), chat.Messages)
	}
	if len(chat.Messages[1].ToolCalls) != 1 ||
		chat.Messages[1].ToolCalls[0].Function.Name != "mcp__tavily__tavily_search" {
		t.Errorf("全限定名未原样透传到 tool_call: %+v", chat.Messages[1])
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "call_9" {
		t.Errorf("function_call_output 未正确映射: %+v", chat.Messages[2])
	}
}

func TestRequestReasoningRoundTrip(t *testing.T) {
	// 多轮 reasoning 回传：reasoning 项的 summary 文本要拼起来并挂到紧随其后的
	// assistant 消息(此处是 function_call)上，否则思考模型上游会报
	// "reasoning_content in the thinking mode must be passed back"。
	// 同时连续的 function_call(并行调用)必须合并进同一条 assistant.tool_calls。
	body := `{"model":"m","input":[
      {"role":"user","content":"天气?"},
      {"type":"reasoning","summary":[{"type":"summary_text","text":"思考A"},{"type":"summary_text","text":"思考B"}]},
      {"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"},
      {"type":"function_call","call_id":"c2","name":"get_air","arguments":"{}"},
      {"type":"function_call_output","call_id":"c1","output":"晴"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	// user + 一条合并了两次并行调用的 assistant + 一条 tool。
	if len(chat.Messages) != 3 {
		t.Fatalf("消息数 = %d, 期望 3: %+v", len(chat.Messages), chat.Messages)
	}
	asst := chat.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("第二条应为 assistant: %+v", asst)
	}
	if asst.ReasoningContent != "思考A思考B" {
		t.Errorf("reasoning summary 未拼接并回传, 实际 %q", asst.ReasoningContent)
	}
	if len(asst.ToolCalls) != 2 {
		t.Errorf("并行 function_call 未合并进同一条 assistant.tool_calls: %+v", asst.ToolCalls)
	}
}

func TestRequestReasoningBeforeMessage(t *testing.T) {
	// reasoning 项紧跟一条 assistant 文本消息时，reasoning 文本应挂到该消息上。
	body := `{"model":"m","input":[
      {"type":"reasoning","summary":[{"type":"summary_text","text":"先想"}]},
      {"role":"assistant","content":"答案"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("消息数 = %d, 期望 1: %+v", len(chat.Messages), chat.Messages)
	}
	m := chat.Messages[0]
	if m.Role != "assistant" || m.Content != "答案" || m.ReasoningContent != "先想" {
		t.Errorf("reasoning 未挂到后续 assistant 消息: %+v", m)
	}
}

func TestRequestContentBlockArray(t *testing.T) {
	// message.content 与 function_call_output.output 都可能是内容块数组，
	// extractText 应拼接其中所有文本块,而不是丢弃。
	body := `{"model":"m","input":[
      {"role":"user","content":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]},
      {"type":"function_call_output","call_id":"c1","output":[{"type":"output_text","text":"x"},{"type":"output_text","text":"y"}]}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("消息数 = %d, 期望 2: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0].Content != "ab" {
		t.Errorf("user content 数组未拼接, 实际 %q", chat.Messages[0].Content)
	}
	if chat.Messages[1].Role != "tool" || chat.Messages[1].Content != "xy" {
		t.Errorf("function_call_output 数组未拼接: %+v", chat.Messages[1])
	}
}

func TestRequestImageContent(t *testing.T) {
	body := `{"model":"m","input":[
      {"role":"user","content":[
        {"type":"input_text","text":"before"},
        {"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"original"},
        {"type":"input_text","text":"after"}
      ]},
      {"type":"input_image","file_id":"file_image_1","detail":"low"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 {
		t.Fatalf("message count = %d, want 2: %+v", len(chat.Messages), chat.Messages)
	}
	first := chat.Messages[0]
	if first.Content != "beforeafter" || len(first.ContentParts) != 3 {
		t.Fatalf("multimodal content = %+v", first)
	}
	if image := first.ContentParts[1].ImageURL; image == nil || image.URL != "data:image/png;base64,AA==" || image.Detail != "high" {
		t.Fatalf("message image = %+v", image)
	}
	second := chat.Messages[1]
	if len(second.ContentParts) != 1 || second.ContentParts[0].ImageURL == nil ||
		second.ContentParts[0].ImageURL.FileID != "file_image_1" || second.ContentParts[0].ImageURL.Detail != "low" {
		t.Fatalf("standalone image = %+v", second)
	}

	wire, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"content":[{"type":"text","text":"before"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}},{"type":"text","text":"after"}]`) {
		t.Fatalf("multimodal request was not serialized as Chat content parts: %s", wire)
	}
	if !strings.Contains(string(wire), `"image_url":{"file_id":"file_image_1","detail":"low"}`) {
		t.Fatalf("file-backed image was not preserved: %s", wire)
	}
}

func TestRequestToolOutputImages(t *testing.T) {
	body := `{"model":"m","input":[
      {"type":"function_call","call_id":"c1","name":"capture","arguments":"{}"},
      {"type":"custom_tool_call","call_id":"c2","name":"render","input":"draw"},
      {"type":"tool_search_call","call_id":"c3","name":"tool_search","arguments":{"query":"image"}},
      {"type":"function_call_output","call_id":"c1","output":[
        {"type":"input_text","text":"screenshot"},
        {"type":"input_image","image_url":"data:image/png;base64,FIRST","detail":"high"}
      ]},
      {"type":"custom_tool_call_output","call_id":"c2","output":"{\"content\":[{\"type\":\"image_url\",\"image_url\":{\"url\":\"https://example.com/render.png\"}}]}"},
      {"type":"tool_search_output","call_id":"c3","status":"completed","output":{"content":[
        {"type":"image","mimeType":"image/webp","data":"THIRD"}
      ]}}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 5 {
		t.Fatalf("message count = %d, want assistant + 3 tools + media: %+v", len(chat.Messages), chat.Messages)
	}
	for i := 1; i <= 3; i++ {
		if chat.Messages[i].Role != "tool" {
			t.Fatalf("message %d role = %q, want tool", i, chat.Messages[i].Role)
		}
	}
	media := chat.Messages[4]
	if media.Role != "user" || len(media.ContentParts) != 6 {
		t.Fatalf("synthetic media message = %+v", media)
	}
	wantURLs := []string{
		"data:image/png;base64,FIRST",
		"https://example.com/render.png",
		"data:image/webp;base64,THIRD",
	}
	for i, want := range wantURLs {
		image := media.ContentParts[i*2+1].ImageURL
		if image == nil || image.URL != want {
			t.Fatalf("media image %d = %+v, want %q", i, image, want)
		}
	}
	for i := 1; i <= 3; i++ {
		if strings.Contains(chat.Messages[i].Content, "FIRST") ||
			strings.Contains(chat.Messages[i].Content, "render.png") ||
			strings.Contains(chat.Messages[i].Content, "THIRD") {
			t.Fatalf("tool message %d still contains image payload: %s", i, chat.Messages[i].Content)
		}
	}
	if !strings.Contains(chat.Messages[3].Content, toolResultMediaMovedMarker) {
		t.Fatalf("tool_search cleaned output missing marker: %s", chat.Messages[3].Content)
	}
}

func TestRequestRawDataURLToolOutput(t *testing.T) {
	body := `{"model":"m","input":[
      {"type":"function_call","call_id":"c1","name":"capture","arguments":"{}"},
      {"type":"function_call_output","call_id":"c1","output":"data:image/gif;base64,R0lGODlh"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 3 || chat.Messages[1].Content != toolResultMediaMovedMarker {
		t.Fatalf("raw data URL tool output = %+v", chat.Messages)
	}
	image := chat.Messages[2].ContentParts[1].ImageURL
	if image == nil || image.URL != "data:image/gif;base64,R0lGODlh" {
		t.Fatalf("raw data URL image = %+v", image)
	}
}

func TestRequestAnthropicToolOutputImage(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"IMAGE"}}]}`)
	plan := planToolOutputMedia(raw)
	if plan == nil || len(plan.media) != 1 {
		t.Fatalf("Anthropic image plan = %+v", plan)
	}
	image := plan.media[0].ImageURL
	if image == nil || image.URL != "data:image/jpeg;base64,IMAGE" {
		t.Fatalf("Anthropic image = %+v", image)
	}
	if strings.Contains(plan.content, "IMAGE") || !strings.Contains(plan.content, toolResultMediaMovedMarker) {
		t.Fatalf("cleaned tool content = %s", plan.content)
	}
}

func TestRequestDeveloperRole(t *testing.T) {
	// Not all Chat Completions providers accept the developer role, so use user.
	body := `{"model":"m","input":[{"role":"developer","content":"开发者指令"}]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Role != "user" {
		t.Errorf("developer 角色未归一为 user: %+v", chat.Messages)
	}
}

func TestRequestDefersMessagesUntilToolOutputs(t *testing.T) {
	body := `{"model":"m","instructions":"base","input":[
      {"type":"function_call","call_id":"c1","name":"one","arguments":"{}"},
      {"type":"function_call","call_id":"c2","name":"two","arguments":"{}"},
      {"role":"developer","content":"developer update"},
      {"type":"function_call_output","call_id":"c1","output":"one done"},
      {"role":"user","content":"approval saved"},
      {"type":"function_call_output","call_id":"c2","output":"two done"},
      {"role":"user","content":"next"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(chat.Messages) != 7 {
		t.Fatalf("消息数 = %d, 期望 7: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "base" {
		t.Fatalf("base instructions 未保留在首位: %+v", chat.Messages)
	}
	if chat.Messages[1].Role != "assistant" || len(chat.Messages[1].ToolCalls) != 2 {
		t.Fatalf("并行工具调用未合并: %+v", chat.Messages[1])
	}
	if chat.Messages[2].Role != "tool" || chat.Messages[2].ToolCallID != "c1" ||
		chat.Messages[3].Role != "tool" || chat.Messages[3].ToolCallID != "c2" {
		t.Fatalf("tool output 未保持邻接: %+v", chat.Messages)
	}
	if chat.Messages[4].Role != "user" || chat.Messages[4].Content != "developer update" ||
		chat.Messages[5].Role != "user" || chat.Messages[5].Content != "approval saved" ||
		chat.Messages[6].Role != "user" || chat.Messages[6].Content != "next" {
		t.Fatalf("延迟消息未按原顺序释放: %+v", chat.Messages)
	}
}

func TestRequestDoesNotDeferMessagesWithoutToolOutput(t *testing.T) {
	body := `{"model":"m","instructions":"base","input":[
      {"type":"function_call","call_id":"c1","name":"one","arguments":"{}"},
      {"role":"developer","content":"developer update"},
      {"role":"user","content":"next"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(chat.Messages) != 4 {
		t.Fatalf("消息数 = %d, 期望 4: %+v", len(chat.Messages), chat.Messages)
	}
	if chat.Messages[2].Role != "user" || chat.Messages[2].Content != "developer update" ||
		chat.Messages[3].Role != "user" || chat.Messages[3].Content != "next" {
		t.Fatalf("无 tool output 时消息顺序不对: %+v", chat.Messages)
	}
}

func TestResponseContentFilter(t *testing.T) {
	// finish_reason=content_filter 应报 incomplete + reason content_filter，
	// 而不是谎报 completed。
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []protocol.ChatChoice{{
			Message:      protocol.ChatMessage{Role: "assistant", Content: "被过滤"},
			FinishReason: "content_filter",
		}},
	}
	resp := ChatToResponses(chat, nil)
	if resp.Status != "incomplete" {
		t.Errorf("status = %q, 期望 incomplete", resp.Status)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "content_filter" {
		t.Errorf("incomplete_details 不对: %+v", resp.IncompleteDetails)
	}
}

// P4: 流式开场应连发 response.created → response.in_progress，且两者都带
// created_at(schema 必填)与 usage 占位；收尾 response.completed 也应带 created_at。
func TestStreamCreatedAndInProgress(t *testing.T) {
	raw := "data: " + `{"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err != nil {
		t.Fatal(err)
	}
	out := s.b.String()

	i := strings.Index(out, "event: response.created")
	j := strings.Index(out, "event: response.in_progress")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("应先发 response.created 再发 response.in_progress, 输出:\n%s", out)
	}
	// 开场对象需带 created_at；usage 此刻为 null，与官方规范一致。
	for _, want := range []string{`"created_at":`, `"usage":null`} {
		if !strings.Contains(out[:strings.Index(out, "event: response.output")], want) {
			t.Errorf("开场事件缺少 %s, 输出:\n%s", want, out)
		}
	}
	// 收尾事件也应带 created_at。
	if tail := out[strings.Index(out, "event: response.completed"):]; !strings.Contains(tail, `"created_at":`) {
		t.Errorf("response.completed 缺少 created_at, 输出:\n%s", out)
	}
}

func TestStreamSequenceNumbersAndKeyFields(t *testing.T) {
	raw := "data: " + `{"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err != nil {
		t.Fatal(err)
	}
	payloads := parseSSEPayloads(t, s.b.String())
	if len(payloads) == 0 {
		t.Fatal("expected SSE payloads")
	}
	for i, payload := range payloads {
		if got := int(payload["sequence_number"].(float64)); got != i+1 {
			t.Fatalf("payload %d sequence_number = %d, want %d", i, got, i+1)
		}
	}
	created := payloads[0]["response"].(map[string]any)
	for _, key := range []string{"background", "error", "incomplete_details", "usage"} {
		if _, ok := created[key]; !ok {
			t.Errorf("response.created missing %q", key)
		}
	}
	var textDelta map[string]any
	for _, payload := range payloads {
		if payload["type"] == "response.output_text.delta" {
			textDelta = payload
			break
		}
	}
	if textDelta == nil {
		t.Fatal("missing response.output_text.delta")
	}
	if _, ok := textDelta["logprobs"]; !ok {
		t.Error("response.output_text.delta missing logprobs")
	}
}

func TestStreamClosesItemsBeforeNextType(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
	}
	raw := "data: " + strings.Join(chunks, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"
	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err != nil {
		t.Fatal(err)
	}
	payloads := parseSSEPayloads(t, s.b.String())
	eventIndex := func(eventType string, outputIndex int) int {
		for i, payload := range payloads {
			if payload["type"] == eventType && int(payload["output_index"].(float64)) == outputIndex {
				return i
			}
		}
		return -1
	}
	reasoningDone := eventIndex("response.output_item.done", 0)
	messageAdded := eventIndex("response.output_item.added", 1)
	messageDone := eventIndex("response.output_item.done", 1)
	toolAdded := eventIndex("response.output_item.added", 2)
	if reasoningDone < 0 || messageAdded < 0 || reasoningDone > messageAdded {
		t.Fatalf("reasoning item must close before message starts:\n%s", s.b.String())
	}
	if messageDone < 0 || toolAdded < 0 || messageDone > toolAdded {
		t.Fatalf("message item must close before tool starts:\n%s", s.b.String())
	}
}

func TestStreamPrematureEOFIsFailure(t *testing.T) {
	raw := "data: " + `{"choices":[{"delta":{"content":"partial"}}]}` + "\n\n"
	var s sink
	_, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil)
	if err == nil {
		t.Fatal("premature EOF should return an error")
	}
	out := s.b.String()
	if !strings.Contains(out, "event: response.failed") {
		t.Fatalf("premature EOF should emit response.failed:\n%s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Fatalf("premature EOF must not emit response.completed:\n%s", out)
	}
}

func TestStreamEOFAfterFinishReasonIsCompatible(t *testing.T) {
	raw := "data: " + `{"choices":[{"delta":{"content":"done"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err != nil {
		t.Fatalf("EOF after finish_reason should remain compatible: %v", err)
	}
	if !strings.Contains(s.b.String(), "event: response.completed") {
		t.Fatalf("missing response.completed:\n%s", s.b.String())
	}
}

func TestStreamReasoningOnlyIsFailure(t *testing.T) {
	raw := "data: " + `{"choices":[{"delta":{"reasoning_content":"unfinished thought"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var s sink
	_, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil)
	if err == nil {
		t.Fatal("reasoning-only stream should return an error")
	}
	out := s.b.String()
	if !strings.Contains(out, "event: response.failed") {
		t.Fatalf("reasoning-only stream should emit response.failed:\n%s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Fatalf("reasoning-only stream must not emit response.completed:\n%s", out)
	}
}

func TestStreamWriteError(t *testing.T) {
	want := errors.New("client write failed")
	result, err := StreamChatToResponses(context.Background(), failingSink{err: want}, strings.NewReader("data: [DONE]\n\n"), "m", nil)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want client write failure", err)
	}
	if result == nil {
		t.Fatal("StreamResult should preserve partial state on write failure")
	}
}

func TestStreamReadError(t *testing.T) {
	// A mid-stream read error arrives after response.created has been sent, so
	// the converter must emit response.failed as the closing event and surface
	// the error to the caller; otherwise the client hangs waiting for an end.
	r := io.MultiReader(
		strings.NewReader("data: "+`{"choices":[{"delta":{"content":"hi"}}]}`+"\n\n"),
		iotest.ErrReader(errors.New("connection reset")),
	)
	var s sink
	result, err := StreamChatToResponses(context.Background(), &s, r, "m", nil)
	if err == nil {
		t.Fatal("read error should be returned to the caller")
	}
	if result == nil {
		t.Fatal("StreamResult should be non-nil on error")
	}
	out := s.b.String()
	if !strings.Contains(out, "event: response.failed") {
		t.Errorf("读流出错应补发 response.failed, 输出前缀:\n%.200s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Errorf("出错时不应发 response.completed")
	}
}

func TestStreamLongLine(t *testing.T) {
	// A single SSE line far larger than any fixed scanner buffer (e.g. a huge
	// tool-call argument delta) must be forwarded, not kill the stream.
	long := strings.Repeat("a", 2*1024*1024)
	raw := "data: " + `{"choices":[{"delta":{"content":"` + long + `"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err != nil {
		t.Fatalf("oversized line should stream fine, got: %v", err)
	}
	out := s.b.String()
	if !strings.Contains(out, "event: response.completed") {
		t.Errorf("missing response.completed, output prefix:\n%.200s", out)
	}
	if !strings.Contains(out, long) {
		t.Error("oversized delta content should be forwarded verbatim")
	}
}

func TestResponseToolsAndReasoning(t *testing.T) {
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{
				Role:             "assistant",
				ReasoningContent: "先想一下",
				ToolCalls: []protocol.ChatToolCall{{
					ID: "call_9", Type: "function",
					Function: protocol.ChatToolCallFunction{Name: "get_weather", Arguments: `{"city":"上海"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	resp := ChatToResponses(chat, nil)
	if len(resp.Output) != 2 {
		t.Fatalf("output 项数 = %d, 期望 2(reasoning+function_call): %+v", len(resp.Output), resp.Output)
	}
	if resp.Output[0].Type != "reasoning" || resp.Output[0].Summary[0].Text != "先想一下" {
		t.Errorf("reasoning 输出项不对: %+v", resp.Output[0])
	}
	fc := resp.Output[1]
	if fc.Type != "function_call" || fc.CallID != "call_9" || fc.Name != "get_weather" {
		t.Errorf("function_call 输出项不对: %+v", fc)
	}
}

func TestResponseToolSearchRestored(t *testing.T) {
	tools := ToolContext{
		"tool_search": {Kind: "tool_search", Name: "tool_search", Execution: "client"},
	}
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-search", Created: 1, Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{Role: "assistant", ToolCalls: []protocol.ChatToolCall{{
				ID: "call_search", Type: "function",
				Function: protocol.ChatToolCallFunction{Name: "tool_search", Arguments: `{"query":"filesystem","limit":4}`},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	resp := ChatToResponses(chat, tools)
	if len(resp.Output) != 1 {
		t.Fatalf("output count = %d, want 1: %+v", len(resp.Output), resp.Output)
	}
	call := resp.Output[0]
	if call.Type != "tool_search_call" || call.CallID != "call_search" || call.Execution != "client" || !strings.HasPrefix(call.ID, "tsc_") {
		t.Fatalf("tool_search identity = %+v", call)
	}
	if call.Name != "" {
		t.Fatalf("tool_search must not expose a function name: %+v", call)
	}
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args.Query != "filesystem" || args.Limit != 4 {
		t.Fatalf("tool_search arguments = %+v", args)
	}
	wire, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), `"arguments":"`) {
		t.Fatalf("tool_search arguments must be an object: %s", wire)
	}
}

func TestResponseCustomToolRestored(t *testing.T) {
	tools := ToolContext{
		"apply_patch": {Kind: "custom", Name: "apply_patch"},
	}
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{Role: "assistant", ToolCalls: []protocol.ChatToolCall{{
				ID: "call_9", Type: "function",
				Function: protocol.ChatToolCallFunction{
					Name:      "apply_patch",
					Arguments: wrapCustomToolInput("*** Begin Patch\n*** End Patch"),
				},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	resp := ChatToResponses(chat, tools)
	if len(resp.Output) != 1 {
		t.Fatalf("output count = %d, want 1: %+v", len(resp.Output), resp.Output)
	}
	call := resp.Output[0]
	if call.Type != "custom_tool_call" || call.CallID != "call_9" || call.Name != "apply_patch" {
		t.Fatalf("custom tool identity = %+v", call)
	}
	if call.Input == nil || *call.Input != "*** Begin Patch\n*** End Patch" || len(call.Arguments) != 0 {
		t.Fatalf("custom input was not restored: %+v", call)
	}
	if !strings.HasPrefix(call.ID, "ctc_") {
		t.Fatalf("custom item id = %q, want ctc_ prefix", call.ID)
	}
}

func TestResponseCustomToolKeepsEmptyInput(t *testing.T) {
	tools := ToolContext{
		"custom": {Kind: "custom", Name: "custom"},
	}
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-empty", Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{Role: "assistant", ToolCalls: []protocol.ChatToolCall{{
				ID:       "call_empty",
				Function: protocol.ChatToolCallFunction{Name: "custom", Arguments: `{"input":""}`},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	wire, err := json.Marshal(ChatToResponses(chat, tools))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"type":"custom_tool_call"`) ||
		!strings.Contains(string(wire), `"input":""`) {
		t.Fatalf("empty custom input must remain present: %s", wire)
	}
}

func TestUnwrapCustomToolInputFallback(t *testing.T) {
	for _, tt := range []struct {
		name string
		args string
		want string
	}{
		{name: "wrapped", args: `{"input":"pwd"}`, want: "pwd"},
		{name: "raw", args: "pwd", want: "pwd"},
		{name: "other object", args: `{"command":"pwd"}`, want: `{"command":"pwd"}`},
		{name: "empty", args: "", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := unwrapCustomToolInput(tt.args); got != tt.want {
				t.Fatalf("unwrapCustomToolInput(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestStreamCustomToolRestored(t *testing.T) {
	tools := ToolContext{
		"apply_patch": {Kind: "custom", Name: "apply_patch"},
	}
	raw := "data: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin"}}]}}]}` +
		"\n\ndata: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" Patch***\"}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n\ndata: [DONE]\n\n"

	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", tools); err != nil {
		t.Fatal(err)
	}
	payloads := parseSSEPayloads(t, s.b.String())
	seen := map[string]map[string]any{}
	for _, payload := range payloads {
		if typ, _ := payload["type"].(string); typ != "" {
			seen[typ] = payload
		}
	}
	added := seen["response.output_item.added"]
	if added == nil {
		t.Fatal("missing response.output_item.added")
	}
	addedItem := added["item"].(map[string]any)
	if addedItem["type"] != "custom_tool_call" || addedItem["name"] != "apply_patch" {
		t.Fatalf("custom added item = %+v", addedItem)
	}
	delta := seen["response.custom_tool_call_input.delta"]
	if delta == nil || delta["delta"] != "*** Begin Patch***" {
		t.Fatalf("custom input delta = %+v", delta)
	}
	done := seen["response.custom_tool_call_input.done"]
	if done == nil || done["input"] != "*** Begin Patch***" {
		t.Fatalf("custom input done = %+v", done)
	}
	itemDone := seen["response.output_item.done"]
	if itemDone == nil {
		t.Fatal("missing response.output_item.done")
	}
	doneItem := itemDone["item"].(map[string]any)
	if doneItem["type"] != "custom_tool_call" || doneItem["input"] != "*** Begin Patch***" {
		t.Fatalf("custom completed item = %+v", doneItem)
	}
	if strings.Contains(s.b.String(), "response.function_call_arguments") {
		t.Fatalf("custom tool must not emit function argument events:\n%s", s.b.String())
	}
}

func TestStreamToolSearchRestored(t *testing.T) {
	tools := ToolContext{
		"tool_search": {Kind: "tool_search", Name: "tool_search", Execution: "client"},
	}
	raw := "data: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_search","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"file"}}]}}]}` +
		"\n\ndata: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"system\",\"limit\":2}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n\ndata: [DONE]\n\n"

	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", tools); err != nil {
		t.Fatal(err)
	}
	payloads := parseSSEPayloads(t, s.b.String())
	var addedItem, doneItem map[string]any
	for _, payload := range payloads {
		switch payload["type"] {
		case "response.output_item.added":
			addedItem, _ = payload["item"].(map[string]any)
		case "response.output_item.done":
			doneItem, _ = payload["item"].(map[string]any)
		}
	}
	if addedItem == nil || addedItem["type"] != "tool_search_call" || addedItem["execution"] != "client" {
		t.Fatalf("tool_search added item = %+v", addedItem)
	}
	if doneItem == nil || doneItem["type"] != "tool_search_call" || doneItem["call_id"] != "call_search" {
		t.Fatalf("tool_search done item = %+v", doneItem)
	}
	args, _ := doneItem["arguments"].(map[string]any)
	if args["query"] != "filesystem" || args["limit"] != float64(2) {
		t.Fatalf("tool_search done arguments = %+v", args)
	}
	if strings.Contains(s.b.String(), "response.function_call_arguments") || strings.Contains(s.b.String(), "response.custom_tool_call_input") {
		t.Fatalf("tool_search must not emit function/custom argument events:\n%s", s.b.String())
	}
}

func TestStreamCustomToolWaitsForLateName(t *testing.T) {
	tools := ToolContext{
		"apply_patch": {Kind: "custom", Name: "apply_patch"},
	}
	raw := "data: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"input\":\"pwd"}}]}}]}` +
		"\n\ndata: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"apply_patch","arguments":"\"}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n\ndata: [DONE]\n\n"

	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", tools); err != nil {
		t.Fatal(err)
	}
	payloads := parseSSEPayloads(t, s.b.String())
	for _, payload := range payloads {
		if payload["type"] != "response.output_item.added" {
			continue
		}
		item := payload["item"].(map[string]any)
		if item["type"] != "custom_tool_call" || item["name"] != "apply_patch" || item["call_id"] != "call_1" {
			t.Fatalf("late-name custom added item = %+v", item)
		}
		return
	}
	t.Fatalf("missing custom output_item.added:\n%s", s.b.String())
}

func TestStreamCustomToolSynthesizesMissingCallID(t *testing.T) {
	tools := ToolContext{
		"apply_patch": {Kind: "custom", Name: "apply_patch"},
	}
	raw := "data: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"apply_patch","arguments":"{\"input\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n\ndata: [DONE]\n\n"

	var s sink
	result, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCallIDs) != 1 || !strings.HasPrefix(result.ToolCallIDs[0], "call_") {
		t.Fatalf("synthesized call IDs = %+v", result.ToolCallIDs)
	}
	if !strings.Contains(s.b.String(), `"type":"custom_tool_call"`) ||
		!strings.Contains(s.b.String(), `"call_id":"`+result.ToolCallIDs[0]+`"`) {
		t.Fatalf("synthesized custom call ID missing from stream:\n%s", s.b.String())
	}
}

func TestStreamToolWithoutNameFails(t *testing.T) {
	raw := "data: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` +
		"\n\ndata: [DONE]\n\n"

	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err == nil {
		t.Fatal("tool call without a name should fail")
	}
	if !strings.Contains(s.b.String(), "event: response.failed") ||
		strings.Contains(s.b.String(), "event: response.completed") {
		t.Fatalf("missing-name tool call should fail without completing:\n%s", s.b.String())
	}
}

// P2: 上游 completion_tokens_details.reasoning_tokens 映射为 output_tokens_details。
func TestResponseUsageReasoningTokens(t *testing.T) {
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-x", Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop",
		}},
		Usage: &protocol.ChatUsage{
			PromptTokens: 10, CompletionTokens: 8, TotalTokens: 18,
			CompletionTokensDetails: &protocol.ChatCompletionTokensDetails{ReasoningTokens: 5},
		},
	}
	resp := ChatToResponses(chat, nil)
	if resp.Usage == nil || resp.Usage.OutputTokensDetails == nil {
		t.Fatalf("output_tokens_details 缺失: %+v", resp.Usage)
	}
	if resp.Usage.OutputTokensDetails.ReasoningTokens != 5 {
		t.Errorf("reasoning_tokens = %d, 期望 5", resp.Usage.OutputTokensDetails.ReasoningTokens)
	}
}

// P3: 上游用 reasoning 字符串(非 reasoning_content)时也应产出 reasoning 输出项。
func TestResponseReasoningFromAltField(t *testing.T) {
	raw := `{"id":"chatcmpl-x","model":"m","choices":[{"index":0,
		"message":{"role":"assistant","reasoning":"另一种字段","content":"答"},
		"finish_reason":"stop"}]}`
	var chat protocol.ChatResponse
	if err := json.Unmarshal([]byte(raw), &chat); err != nil {
		t.Fatal(err)
	}
	resp := ChatToResponses(&chat, nil)
	if len(resp.Output) != 2 || resp.Output[0].Type != "reasoning" {
		t.Fatalf("应有 reasoning + message 两项: %+v", resp.Output)
	}
	if resp.Output[0].Summary[0].Text != "另一种字段" {
		t.Errorf("reasoning 文本 = %q", resp.Output[0].Summary[0].Text)
	}
}

// P5: 上游把非流式 message.content 返回成内容块数组时不应崩溃，文本应被拼接。
func TestResponseContentBlockArray(t *testing.T) {
	raw := `{"id":"chatcmpl-x","model":"m","choices":[{"index":0,
		"message":{"role":"assistant","content":[
			{"type":"text","text":"前"},{"type":"text","text":"后"}]},
		"finish_reason":"stop"}]}`
	var chat protocol.ChatResponse
	if err := json.Unmarshal([]byte(raw), &chat); err != nil {
		t.Fatalf("数组形态 content 不应反序列化失败: %v", err)
	}
	resp := ChatToResponses(&chat, nil)
	if len(resp.Output) != 1 || resp.Output[0].Type != "message" {
		t.Fatalf("应有一条 message 输出项: %+v", resp.Output)
	}
	if got := resp.Output[0].Content[0].Text; got != "前后" {
		t.Errorf("content 数组未拼接, 实际 %q", got)
	}
}

// P5: 上游返回 refusal(content 缺省)时不应崩溃，应映射为 refusal 内容块。
func TestResponseRefusal(t *testing.T) {
	raw := `{"id":"chatcmpl-x","model":"m","choices":[{"index":0,
		"message":{"role":"assistant","refusal":"我不能这么做"},
		"finish_reason":"stop"}]}`
	var chat protocol.ChatResponse
	if err := json.Unmarshal([]byte(raw), &chat); err != nil {
		t.Fatalf("refusal 形态不应反序列化失败: %v", err)
	}
	resp := ChatToResponses(&chat, nil)
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 {
		t.Fatalf("应有一条含单内容块的 message: %+v", resp.Output)
	}
	c := resp.Output[0].Content[0]
	if c.Type != "refusal" || c.Refusal != "我不能这么做" {
		t.Errorf("refusal 内容块不对: %+v", c)
	}
}

func TestResponseIncompleteOnLength(t *testing.T) {
	// 上游因 max_tokens 截断(finish_reason=length)，整体状态与 message 项都应是 incomplete，
	// 并带 incomplete_details.reason=max_output_tokens，而不是谎报 completed。
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []protocol.ChatChoice{{
			Message:      protocol.ChatMessage{Role: "assistant", Content: "被截断的文"},
			FinishReason: "length",
		}},
	}
	resp := ChatToResponses(chat, nil)
	if resp.Status != "incomplete" {
		t.Errorf("status = %q, 期望 incomplete", resp.Status)
	}
	if resp.IncompleteDetails == nil || resp.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("incomplete_details 不对: %+v", resp.IncompleteDetails)
	}
	if len(resp.Output) != 1 || resp.Output[0].Status != "incomplete" {
		t.Errorf("message 项状态应为 incomplete: %+v", resp.Output)
	}
}

func TestResponseCompletedNoIncompleteDetails(t *testing.T) {
	// 正常结束(finish_reason=stop)：completed 且不带 incomplete_details。
	chat := &protocol.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []protocol.ChatChoice{{
			Message:      protocol.ChatMessage{Role: "assistant", Content: "完整回答"},
			FinishReason: "stop",
		}},
	}
	resp := ChatToResponses(chat, nil)
	if resp.Status != "completed" || resp.IncompleteDetails != nil {
		t.Errorf("正常结束应为 completed 且无 incomplete_details: status=%q details=%+v",
			resp.Status, resp.IncompleteDetails)
	}
}

func TestStreamIncompleteOnLength(t *testing.T) {
	// 流式截断：末尾分片带 finish_reason=length，收尾应发 response.incomplete 而非 completed。
	raw := "data: " + `{"choices":[{"delta":{"content":"半句"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err != nil {
		t.Fatal(err)
	}
	out := s.b.String()
	if !strings.Contains(out, "event: response.incomplete") {
		t.Errorf("缺少 response.incomplete 事件, 输出:\n%s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Errorf("被截断时不应再发 response.completed, 输出:\n%s", out)
	}
	if !strings.Contains(out, `"reason":"max_output_tokens"`) {
		t.Errorf("缺少 incomplete_details.reason, 输出:\n%s", out)
	}
}

func TestStreamUpstreamError(t *testing.T) {
	// 上游在流中途返回 error 对象：应补发 response.failed，而不是静默吞掉让客户端挂死。
	raw := "data: " + `{"choices":[{"delta":{"content":"开头"}}]}` + "\n\n" +
		"data: " + `{"error":{"message":"upstream blew up","type":"server_error"}}` + "\n\n"
	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil); err == nil {
		t.Fatal("upstream stream error should be returned to the caller for logging/stats")
	}
	out := s.b.String()
	if !strings.Contains(out, "event: response.failed") {
		t.Errorf("缺少 response.failed 事件, 输出:\n%s", out)
	}
	if !strings.Contains(out, "upstream blew up") {
		t.Errorf("response.failed 应带上游错误信息, 输出:\n%s", out)
	}
	if strings.Contains(out, "event: response.completed") {
		t.Errorf("出错时不应发 response.completed, 输出:\n%s", out)
	}
}

func TestResponseNamespaceRestored(t *testing.T) {
	// 上游用扁平名发回工具调用，apid 应把它拆回本地名 + namespace，
	// 否则 Codex 按 {name, namespace} 匹配注册表会失败(unsupported call)。
	req := &protocol.ResponsesRequest{Tools: []protocol.ResponsesTool{
		{Type: "namespace", Name: "mcp__tavily", Tools: []protocol.ResponsesTool{
			{Type: "function", Name: "tavily_search"},
		}},
	}}
	_, ns, err := ResponsesToChat(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ns["mcp__tavily__tavily_search"]; got.Namespace != "mcp__tavily" || got.Name != "tavily_search" {
		t.Fatalf("命名空间映射错误: %+v", ns)
	}

	chat := &protocol.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []protocol.ChatChoice{{
			Message: protocol.ChatMessage{Role: "assistant", ToolCalls: []protocol.ChatToolCall{{
				ID: "call_1", Type: "function",
				Function: protocol.ChatToolCallFunction{Name: "mcp__tavily__tavily_search", Arguments: `{"q":"x"}`},
			}}},
			FinishReason: "tool_calls",
		}},
	}
	resp := ChatToResponses(chat, ns)
	fc := resp.Output[len(resp.Output)-1]
	// 上游扁平名应拆成本地名 + namespace，与 Codex 注册表 ToolName{name,namespace} 对齐。
	if fc.Type != "function_call" || fc.Name != "tavily_search" {
		t.Fatalf("function_call name 应拆回本地名, 实际: %+v", fc)
	}
	if fc.Namespace != "mcp__tavily" {
		t.Errorf("namespace 未补回, 期望 mcp__tavily, 实际 %q", fc.Namespace)
	}
}

func TestRequestNamespaceCallReflatten(t *testing.T) {
	// 多轮回传：function_call 带本地名 + namespace，应拼回上游的扁平全限定名。
	body := `{"model":"m","input":[
      {"type":"function_call","call_id":"c1","name":"tavily_search","namespace":"mcp__tavily","arguments":"{}"},
      {"type":"function_call_output","call_id":"c1","output":"ok"}
    ]}`
	var req protocol.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := chat.Messages[0].ToolCalls[0].Function.Name; got != "mcp__tavily__tavily_search" {
		t.Errorf("未拼回扁平名, 实际 %q", got)
	}
}

func TestStreamNamespaceRestored(t *testing.T) {
	ns := ToolContext{
		"mcp__tavily__tavily_search": {Namespace: "mcp__tavily", Name: "tavily_search"},
	}
	raw := "data: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"mcp__tavily__tavily_search","arguments":"{}"}}]}}]}` +
		"\n\ndata: [DONE]\n\n"

	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", ns); err != nil {
		t.Fatal(err)
	}
	out := s.b.String()
	// added 与 done 事件里都应带上 namespace，且 name 应是本地名。
	if strings.Count(out, `"namespace":"mcp__tavily"`) < 2 {
		t.Errorf("function_call 事件缺少 namespace 字段, 输出:\n%s", out)
	}
	if !strings.Contains(out, `"name":"tavily_search"`) {
		t.Errorf("function_call name 应拆回本地名 tavily_search, 输出:\n%s", out)
	}
}

func TestStreamResultUsageAndTTFT(t *testing.T) {
	// StreamResult carries the end-of-stream usage and the first-token time;
	// both feed the stats pipeline (billing + TTFT), so they must survive the
	// conversion. The usage chunk has empty choices, as sent by upstreams
	// honoring stream_options.include_usage.
	raw := "data: " + `{"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: " + `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":6},"completion_tokens_details":{"reasoning_tokens":3}}}` + "\n\n" +
		"data: [DONE]\n\n"
	var s sink
	result, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage == nil {
		t.Fatal("StreamResult.Usage should carry the final usage chunk")
	}
	u := result.Usage
	if u.PromptTokens != 10 || u.CompletionTokens != 4 || u.TotalTokens != 14 {
		t.Errorf("usage = %+v, want prompt=10 completion=4 total=14", u)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 6 {
		t.Errorf("cached tokens not preserved: %+v", u.PromptTokensDetails)
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Errorf("reasoning tokens not preserved: %+v", u.CompletionTokensDetails)
	}
	if result.FirstTokenAt.IsZero() {
		t.Error("FirstTokenAt should be set by the first content delta")
	}
	// response.completed must expose usage to the client, cached + reasoning tokens included.
	out := s.b.String()
	for _, want := range []string{`"input_tokens":10`, `"output_tokens":4`, `"total_tokens":14`, `"cached_tokens":6`, `"reasoning_tokens":3`} {
		if !strings.Contains(out, want) {
			t.Errorf("response.completed missing %s, output:\n%s", want, out)
		}
	}
}

func TestStreamResultNoContent(t *testing.T) {
	// A stream with no content deltas (only finish_reason) and no usage chunk:
	// FirstTokenAt must stay zero so callers don't record a bogus TTFT,
	// and Usage must stay nil so stats writes zeros instead of garbage.
	raw := "data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var s sink
	result, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw), "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FirstTokenAt.IsZero() {
		t.Error("FirstTokenAt should stay zero when the stream has no content delta")
	}
	if result.Usage != nil {
		t.Errorf("Usage should stay nil without a usage chunk, got %+v", result.Usage)
	}
}

// chunkReader yields one chunk per Read call, invoking onRead first so a test
// can cancel a context at a precise point mid-stream.
type chunkReader struct {
	chunks []string
	i      int
	onRead func(i int)
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.i >= len(c.chunks) {
		return 0, io.EOF
	}
	if c.onRead != nil {
		c.onRead(c.i)
	}
	n := copy(p, c.chunks[c.i])
	c.i++
	return n, nil
}

func TestStreamClientDisconnect(t *testing.T) {
	// Client gone mid-stream (ctx canceled): the converter must stop instead of
	// draining the upstream, return the context error, keep partial state
	// (FirstTokenAt) in the result, and not emit a closing completed/failed
	// event to the already-disconnected client.
	ctx, cancel := context.WithCancel(context.Background())
	r := &chunkReader{
		chunks: []string{
			"data: " + `{"choices":[{"delta":{"content":"first"}}]}` + "\n\n",
			"data: " + `{"choices":[{"delta":{"content":"second"}}]}` + "\n\n",
		},
		onRead: func(i int) {
			if i == 1 {
				cancel()
			}
		},
	}
	var s sink
	result, err := StreamChatToResponses(ctx, &s, r, "m", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if result == nil {
		t.Fatal("result should be non-nil on disconnect")
	}
	if result.FirstTokenAt.IsZero() {
		t.Error("FirstTokenAt from before the disconnect should be preserved")
	}
	out := s.b.String()
	if !strings.Contains(out, `"delta":"first"`) {
		t.Errorf("delta before disconnect should have been emitted, output:\n%s", out)
	}
	if strings.Contains(out, `"delta":"second"`) {
		t.Errorf("delta after disconnect should not be processed, output:\n%s", out)
	}
	for _, ev := range []string{"response.completed", "response.failed"} {
		if strings.Contains(out, "event: "+ev) {
			t.Errorf("should not emit %s after disconnect, output:\n%s", ev, out)
		}
	}
}

func TestStreamToolsAndReasoning(t *testing.T) {
	// 构造上游 SSE：先 reasoning，再工具调用分片。
	chunks := []string{
		`{"choices":[{"delta":{"reasoning_content":"想"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"一下"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_7","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"沪\"}"}}]}}]}`,
	}
	var raw strings.Builder
	for _, c := range chunks {
		raw.WriteString("data: ")
		raw.WriteString(c)
		raw.WriteString("\n\n")
	}
	raw.WriteString("data: [DONE]\n\n")

	var s sink
	if _, err := StreamChatToResponses(context.Background(), &s, strings.NewReader(raw.String()), "m", nil); err != nil {
		t.Fatal(err)
	}
	out := s.b.String()

	// 关键事件都应出现
	for _, ev := range []string{
		"response.created",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.completed",
	} {
		if !strings.Contains(out, "event: "+ev) {
			t.Errorf("缺少事件 %s", ev)
		}
	}
	// part 生命周期必须包住文本：part.added 在首个 delta 之前，part.done 在 text.done 之后。
	if i, j := strings.Index(out, "event: response.reasoning_summary_part.added"),
		strings.Index(out, "event: response.reasoning_summary_text.delta"); i < 0 || i > j {
		t.Errorf("reasoning_summary_part.added 应在首个 delta 之前, 输出:\n%s", out)
	}
	if i, j := strings.Index(out, "event: response.reasoning_summary_text.done"),
		strings.Index(out, "event: response.reasoning_summary_part.done"); i < 0 || i > j {
		t.Errorf("reasoning_summary_part.done 应在 text.done 之后, 输出:\n%s", out)
	}
	// 完整参数应被拼接出来
	if !strings.Contains(out, `{\"city\":\"沪\"}`) {
		t.Errorf("工具参数未正确拼接, 输出:\n%s", out)
	}
}
