package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
)

func TestOutputTokenDefaultsForwarded(t *testing.T) {
	for _, mode := range []struct {
		name             string
		client, upstream config.Protocol
		dual             bool
		chatField        string
	}{
		{"conversion", config.ProtoResponses, config.ProtoChat, false, ""},
		{"chat", config.ProtoChat, config.ProtoChat, false, ""},
		{"responses", config.ProtoResponses, config.ProtoResponses, false, ""},
		{"dual", config.ProtoResponses, config.ProtoChat, true, ""},
		{"anthropic", config.ProtoAnthropic, config.ProtoAnthropic, false, ""},
		{"conversion-legacy", config.ProtoResponses, config.ProtoChat, false, "max_tokens"},
		{"chat-legacy", config.ProtoChat, config.ProtoChat, false, "max_tokens"},
		{"dual-legacy", config.ProtoResponses, config.ProtoChat, true, "max_tokens"},
	} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, value string
				limit       int
				want        string
			}{
				{name: "disabled"},
				{name: "default", limit: 32768, want: "32768"},
				{name: "null", value: "null", limit: 32768, want: "32768"},
				{name: "explicit", value: "123", limit: 32768, want: "123"},
				{name: "explicit-without-default", value: "123", want: "123"},
				{name: "larger", value: "65536", limit: 32768, want: "65536"},
				{name: "zero", value: "0", limit: 32768, want: "0"},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", mode.name, stream, tc.name), func(t *testing.T) {
					captured := make(chan []byte, 1)
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						captured <- body
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					}))
					defer up.Close()
					cfg := forwardConfig("/test", mode.upstream, up.URL, "/v1/chat/completions", "")
					cfg.Routes[0].InputProtocol = mode.client
					cfg.Upstreams[0].SupportsResponses = mode.dual
					cfg.Upstreams[0].ChatMaxTokensField = mode.chatField
					cfg.Upstreams[0].DefaultMaxOutputTokens = tc.limit
					srv := New(cfg, nil, nil)
					defer srv.Close()

					inputField, outputField := "max_tokens", "max_tokens"
					if mode.client == config.ProtoChat && mode.chatField != "max_tokens" {
						inputField = "max_completion_tokens"
					}
					if mode.upstream == config.ProtoChat && mode.chatField != "max_tokens" {
						outputField = "max_completion_tokens"
					}
					if mode.client == config.ProtoResponses {
						inputField = "max_output_tokens"
					}
					if mode.upstream == config.ProtoResponses || mode.dual {
						outputField = "max_output_tokens"
					}
					body := fmt.Sprintf(`{"model":"m","input":"hi","messages":[],"stream":%t,"extension":{"integer":9007199254740993}`, stream)
					if tc.value != "" {
						body += fmt.Sprintf(`,"%s":%s`, inputField, tc.value)
					}
					body += "}"
					rec := httptest.NewRecorder()
					srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body)))
					if rec.Code != http.StatusOK {
						t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
					}
					raw := <-captured
					var got map[string]json.RawMessage
					if err := json.Unmarshal(raw, &got); err != nil {
						t.Fatal(err)
					}
					if string(got[outputField]) != tc.want {
						t.Fatalf("%s = %s, want %s; body=%s", outputField, got[outputField], tc.want, raw)
					}
					for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
						if _, ok := got[key]; ok && key != outputField {
							t.Fatalf("unexpected token field %s: %s", key, raw)
						}
					}
					if strings.HasPrefix(mode.name, "conversion") {
						if _, ok := got["max_output_tokens"]; ok {
							t.Fatalf("unconverted token field: %s", raw)
						}
					} else {
						if string(got["extension"]) != `{"integer":9007199254740993}` {
							t.Fatalf("extension changed: %s", raw)
						}
						if !stream && (tc.limit == 0 || tc.value != "" && tc.value != "null") && string(raw) != body {
							t.Fatalf("explicit/disabled request changed: %s", raw)
						}
					}
				})
			}
		}
	}
}

func TestChatCompletionTokenAlias(t *testing.T) {
	for _, field := range []string{"max_completion_tokens", "max_tokens"} {
		for _, body := range []string{
			`{"max_completion_tokens":99}`, `{"max_completion_tokens":0}`,
			`{"max_tokens":88}`, `{"max_tokens":0}`,
			`{"max_tokens":null,"max_completion_tokens":99}`,
			`{"max_tokens":88,"max_completion_tokens":null}`,
			`{"max_tokens":88,"max_completion_tokens":99}`,
		} {
			t.Run(field+"/"+body, func(t *testing.T) {
				got, err := defaultOutputTokens([]byte(body), config.ProtoChat, 32768, field)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != body {
					t.Fatalf("got %s, want unchanged %s", got, body)
				}
			})
		}
		for _, body := range []string{`{}`, `{"max_tokens":null}`, `{"max_completion_tokens":null}`, `{"max_tokens":null,"max_completion_tokens":null}`} {
			t.Run(field+"/"+body, func(t *testing.T) {
				got, err := defaultOutputTokens([]byte(body), config.ProtoChat, 32768, field)
				if err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf(`{"%s":32768}`, field)
				if string(got) != want {
					t.Fatalf("got %s, want %s", got, want)
				}
			})
		}
	}
}

func TestOutputTokenDefaultRejectsInvalidBody(t *testing.T) {
	for _, body := range []string{"null", "[]", "invalid", "1"} {
		t.Run(body, func(t *testing.T) {
			cfg := forwardConfig("/test", config.ProtoChat, "http://127.0.0.1:1", "/chat", "")
			cfg.Upstreams[0].DefaultMaxOutputTokens = 32768
			srv := New(cfg, nil, nil)
			defer srv.Close()
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}
