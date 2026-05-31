package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/internal/config"
	"github.com/abowloflrf/apid/internal/types"
)

// mockUpstream 返回一个模拟 Chat Completions 的上游服务器。
func mockUpstream(t *testing.T, stream bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 校验请求确实被转换成了 chat 格式。
		var req types.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("上游收到的请求无法解析: %v", err)
		}
		if len(req.Messages) == 0 {
			t.Errorf("上游收到的 messages 为空")
		}

		if !stream {
			_ = json.NewEncoder(w).Encode(types.ChatResponse{
				ID: "chatcmpl-abc", Object: "chat.completion", Created: 1, Model: req.Model,
				Choices: []types.ChatChoice{{
					Message: types.ChatMessage{Role: "assistant", Content: "你好世界"}, FinishReason: "stop",
				}},
				Usage: &types.ChatUsage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, tok := range []string{"你", "好", "世界"} {
			chunk := types.ChatStreamChunk{
				ID: "chatcmpl-abc", Model: req.Model,
				Choices: []types.ChatStreamChoice{{Delta: types.ChatChunkDelta{Content: tok}}},
			}
			b, _ := json.Marshal(chunk)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
}

func newTestServer(upstreamURL string) http.Handler {
	return New(config.Config{UpstreamBaseURL: upstreamURL}).Handler()
}

func TestNonStreaming(t *testing.T) {
	up := mockUpstream(t, false)
	defer up.Close()
	h := newTestServer(up.URL)

	body := `{"model":"gpt-x","instructions":"你是助手","input":"你好"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp types.ResponsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Errorf("响应顶层字段不对: %+v", resp)
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 {
		t.Fatalf("output 结构不对: %+v", resp.Output)
	}
	if got := resp.Output[0].Content[0].Text; got != "你好世界" {
		t.Errorf("文本 = %q, 期望 你好世界", got)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 8 {
		t.Errorf("usage 不对: %+v", resp.Usage)
	}
}

// TestUpstreamModelOverride 验证配置 UpstreamModel 后，转发给上游的 model 被覆盖。
func TestUpstreamModelOverride(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req types.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		_ = json.NewEncoder(w).Encode(types.ChatResponse{
			ID: "chatcmpl-abc", Object: "chat.completion", Created: 1, Model: req.Model,
			Choices: []types.ChatChoice{{Message: types.ChatMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
		})
	}))
	defer up.Close()

	h := New(config.Config{UpstreamBaseURL: up.URL, UpstreamModel: "real-backend-model"}).Handler()

	body := `{"model":"gpt-x","input":"你好"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "real-backend-model" {
		t.Errorf("上游收到的 model = %q, 期望 real-backend-model", gotModel)
	}
}

func TestStreaming(t *testing.T) {
	up := mockUpstream(t, true)
	defer up.Close()
	h := newTestServer(up.URL)

	body := `{"model":"gpt-x","input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}

	var events []string
	var fullText strings.Builder
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			events = append(events, strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
				if m["type"] == "response.output_text.delta" {
					fullText.WriteString(m["delta"].(string))
				}
			}
		}
	}

	if events[0] != "response.created" {
		t.Errorf("首个事件 = %q, 期望 response.created", events[0])
	}
	if last := events[len(events)-1]; last != "response.completed" {
		t.Errorf("末个事件 = %q, 期望 response.completed", last)
	}
	if fullText.String() != "你好世界" {
		t.Errorf("拼接文本 = %q, 期望 你好世界", fullText.String())
	}
}

var _ = io.Discard
