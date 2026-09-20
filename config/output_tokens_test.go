package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadDefaultMaxOutputTokens(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		want          int
		wantErr       bool
	}{
		{name: "omitted"},
		{name: "disabled", setting: "default_max_output_tokens = 0"},
		{name: "positive", setting: "default_max_output_tokens = 32768", want: 32768},
		{name: "negative", setting: "default_max_output_tokens = -1", wantErr: true},
		{name: "string", setting: `default_max_output_tokens = "32768"`, wantErr: true},
		{name: "fraction", setting: "default_max_output_tokens = 1.5", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTOML(t, fmt.Sprintf(`
[[upstream]]
name = "up"
protocol = "openai_chat_completions"
base_url = "https://example.com"
path = "/v1/chat/completions"
%s
[[route]]
path = "/v1/responses"
input_protocol = "openai_responses"
[[route.model]]
upstream = "up"
`, tc.setting))
			ups, _, err := loadFile(path)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "default_max_output_tokens") {
					t.Fatalf("expected token configuration error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ups[0].DefaultMaxOutputTokens != tc.want {
				t.Fatalf("default = %d, want %d", ups[0].DefaultMaxOutputTokens, tc.want)
			}
		})
	}
}

func TestSubscriptionRejectsDefaultMaxOutputTokens(t *testing.T) {
	u := Upstream{
		Name: "subscription", Protocol: ProtoResponses,
		BaseURL: CodexSubscriptionBaseURL, Path: CodexSubscriptionPath,
		AuthMode: AuthModeCodexSubscription, DefaultMaxOutputTokens: 32768,
	}
	empty := ""
	for _, operation := range []RouteOperation{RouteOperationInference, RouteOperationResponsesCompact} {
		err := validateConfig([]Upstream{u}, []Route{{
			Path: "/responses", InputProtocol: ProtoResponses, Operation: operation,
			Models: []ModelRule{{Match: "*", Upstream: u.Name, Model: &empty}},
		}})
		if err == nil || !strings.Contains(err.Error(), "default_max_output_tokens") {
			t.Fatalf("operation %q: expected token configuration error, got %v", operation, err)
		}
	}
}

func TestLoadChatMaxTokensField(t *testing.T) {
	for _, tc := range []struct {
		name, proto, setting, want string
		wantErr                    bool
	}{
		{name: "omitted", proto: "openai_chat_completions", want: "max_completion_tokens"},
		{name: "empty", proto: "openai_chat_completions", setting: `chat_max_tokens_field = ""`, want: "max_completion_tokens"},
		{name: "modern", proto: "openai_chat_completions", setting: `chat_max_tokens_field = "max_completion_tokens"`, want: "max_completion_tokens"},
		{name: "legacy", proto: "openai_chat_completions", setting: `chat_max_tokens_field = "max_tokens"`, want: "max_tokens"},
		{name: "invalid", proto: "openai_chat_completions", setting: `chat_max_tokens_field = "max_output_tokens"`, wantErr: true},
		{name: "wrong-type", proto: "openai_chat_completions", setting: `chat_max_tokens_field = 1`, wantErr: true},
		{name: "responses", proto: "openai_responses", setting: `chat_max_tokens_field = "max_tokens"`, wantErr: true},
		{name: "anthropic", proto: "anthropic_messages", setting: `chat_max_tokens_field = "max_tokens"`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ups, _, err := loadFile(writeTOML(t, fmt.Sprintf(`
[[upstream]]
name = "up"
protocol = "%s"
base_url = "https://example.com"
path = "/api"
%s
[[route]]
path = "/api"
input_protocol = "%s"
[[route.model]]
upstream = "up"
`, tc.proto, tc.setting, tc.proto)))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "chat_max_tokens_field") {
					t.Fatalf("expected chat token field error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := ups[0].EffectiveChatMaxTokensField(); got != tc.want {
				t.Fatalf("field = %q, want %q", got, tc.want)
			}
		})
	}
}
