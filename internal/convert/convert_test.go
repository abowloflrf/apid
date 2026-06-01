package convert

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/internal/types"
)

// sink 实现 SSEWriter，收集所有写入内容用于断言。
type sink struct{ b strings.Builder }

func (s *sink) Write(p []byte) (int, error) { return s.b.Write(p) }
func (s *sink) Flush()                      {}

func TestRequestTools(t *testing.T) {
	body := `{
      "model":"m",
      "input":"今天天气如何",
      "tools":[{"type":"function","name":"get_weather","description":"查天气","parameters":{"type":"object"}}],
      "tool_choice":{"type":"function","name":"get_weather"},
      "reasoning":{"effort":"high"}
    }`
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req)
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

func TestRequestToolParametersDefault(t *testing.T) {
	// parameters 缺失 / 缺 type / 已带 type 三种情形，发给上游时都应是带 type:object 的 schema。
	body := `{"model":"m","input":"hi","tools":[
      {"type":"function","name":"no_params"},
      {"type":"function","name":"empty_params","parameters":{}},
      {"type":"function","name":"with_props","parameters":{"properties":{"x":{"type":"string"}}}},
      {"type":"function","name":"already_typed","parameters":{"type":"array"}}
    ]}`
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req)
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
		body := `{"model":"m","input":"hi","tool_choice":` + in + `}`
		var req types.ResponsesRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("input %s: %v", in, err)
		}
		chat, _, err := ResponsesToChat(&req)
		if err != nil {
			t.Fatalf("input %s: %v", in, err)
		}
		if got := string(chat.ToolChoice); !strings.Contains(got, want) {
			t.Errorf("tool_choice %s -> %s, 期望含 %s", in, got, want)
		}
	}
}

func TestRequestParallelToolCalls(t *testing.T) {
	body := `{"model":"m","input":"hi","parallel_tool_calls":false}`
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req)
	if err != nil {
		t.Fatal(err)
	}
	if chat.ParallelToolCalls == nil || *chat.ParallelToolCalls != false {
		t.Errorf("parallel_tool_calls 未透传: %+v", chat.ParallelToolCalls)
	}

	// 未提供时不应注入该字段（保持 nil，序列化时省略）。
	body2 := `{"model":"m","input":"hi"}`
	var req2 types.ResponsesRequest
	_ = json.Unmarshal([]byte(body2), &req2)
	chat2, _, _ := ResponsesToChat(&req2)
	if chat2.ParallelToolCalls != nil {
		t.Errorf("未提供时不应设置 parallel_tool_calls, 实际 %+v", chat2.ParallelToolCalls)
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
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req)
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
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req)
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
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req)
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

func TestResponseToolsAndReasoning(t *testing.T) {
	chat := &types.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []types.ChatChoice{{
			Message: types.ChatMessage{
				Role:             "assistant",
				ReasoningContent: "先想一下",
				ToolCalls: []types.ChatToolCall{{
					ID: "call_9", Type: "function",
					Function: types.ChatToolCallFunction{Name: "get_weather", Arguments: `{"city":"上海"}`},
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

func TestResponseIncompleteOnLength(t *testing.T) {
	// 上游因 max_tokens 截断(finish_reason=length)，整体状态与 message 项都应是 incomplete，
	// 并带 incomplete_details.reason=max_output_tokens，而不是谎报 completed。
	chat := &types.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []types.ChatChoice{{
			Message:      types.ChatMessage{Role: "assistant", Content: "被截断的文"},
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
	chat := &types.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []types.ChatChoice{{
			Message:      types.ChatMessage{Role: "assistant", Content: "完整回答"},
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
	if _, err := StreamChatToResponses(&s, strings.NewReader(raw), "m", nil); err != nil {
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
	if _, err := StreamChatToResponses(&s, strings.NewReader(raw), "m", nil); err != nil {
		t.Fatal(err)
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
	req := &types.ResponsesRequest{Tools: []types.ResponsesTool{
		{Type: "namespace", Name: "mcp__tavily", Tools: []types.ResponsesTool{
			{Type: "function", Name: "tavily_search"},
		}},
	}}
	_, ns, err := ResponsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := ns["mcp__tavily__tavily_search"]; got.Namespace != "mcp__tavily" || got.Name != "tavily_search" {
		t.Fatalf("命名空间映射错误: %+v", ns)
	}

	chat := &types.ChatResponse{
		ID: "chatcmpl-x", Created: 1, Model: "m",
		Choices: []types.ChatChoice{{
			Message: types.ChatMessage{Role: "assistant", ToolCalls: []types.ChatToolCall{{
				ID: "call_1", Type: "function",
				Function: types.ChatToolCallFunction{Name: "mcp__tavily__tavily_search", Arguments: `{"q":"x"}`},
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
	var req types.ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	chat, _, err := ResponsesToChat(&req)
	if err != nil {
		t.Fatal(err)
	}
	if got := chat.Messages[0].ToolCalls[0].Function.Name; got != "mcp__tavily__tavily_search" {
		t.Errorf("未拼回扁平名, 实际 %q", got)
	}
}

func TestStreamNamespaceRestored(t *testing.T) {
	ns := map[string]NamespacedTool{
		"mcp__tavily__tavily_search": {Namespace: "mcp__tavily", Name: "tavily_search"},
	}
	raw := "data: " +
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"mcp__tavily__tavily_search","arguments":"{}"}}]}}]}` +
		"\n\ndata: [DONE]\n\n"

	var s sink
	if _, err := StreamChatToResponses(&s, strings.NewReader(raw), "m", ns); err != nil {
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
	if _, err := StreamChatToResponses(&s, strings.NewReader(raw.String()), "m", nil); err != nil {
		t.Fatal(err)
	}
	out := s.b.String()

	// 关键事件都应出现
	for _, ev := range []string{
		"response.created",
		"response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.completed",
	} {
		if !strings.Contains(out, "event: "+ev) {
			t.Errorf("缺少事件 %s", ev)
		}
	}
	// 完整参数应被拼接出来
	if !strings.Contains(out, `{\"city\":\"沪\"}`) {
		t.Errorf("工具参数未正确拼接, 输出:\n%s", out)
	}
}
