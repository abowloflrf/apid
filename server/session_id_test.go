package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestExtractAgentSessionID(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		body    string
		want    string
	}{
		{
			name:    "claude code header",
			headers: map[string]string{"X-Claude-Code-Session-Id": "claude-session"},
			want:    "claude-session",
		},
		{
			name:    "opencode managed header",
			headers: map[string]string{"x-opencode-session": "opencode-session", "x-opencode-client": "cli"},
			want:    "opencode-session",
		},
		{
			name:    "pi through opencode",
			headers: map[string]string{"x-opencode-session": "pi-session", "x-opencode-client": "pi"},
			want:    "pi-session",
		},
		{
			name: "codex canonical body wins",
			headers: map[string]string{
				"session-id": "header-session",
				"thread-id":  "thread-1",
			},
			body: `{"client_metadata":{"session_id":"flat-session","thread_id":"thread-1","x-codex-turn-metadata":"{\"session_id\":\"canonical-session\"}"}}`,
			want: "canonical-session",
		},
		{
			name:    "codex malformed canonical falls back to flat body",
			headers: map[string]string{"session-id": "header-session", "thread-id": "thread-1"},
			body:    `{"client_metadata":{"session_id":"flat-session","x-codex-turn-metadata":"{"}}`,
			want:    "flat-session",
		},
		{
			name: "codex canonical header wins over session header",
			headers: map[string]string{
				"session-id":            "header-session",
				"x-codex-turn-metadata": `{"session_id":"canonical-header-session"}`,
			},
			want: "canonical-header-session",
		},
		{
			name:    "codex session header",
			headers: map[string]string{"session-id": "codex-session", "thread-id": "codex-thread"},
			want:    "codex-session",
		},
		{
			name:    "malformed body does not hide header",
			headers: map[string]string{"session-id": "codex-session", "thread-id": "codex-thread"},
			body:    `{`,
			want:    "codex-session",
		},
		{
			name: "opencode external provider",
			headers: map[string]string{
				"x-session-id":       "opencode-session",
				"x-session-affinity": "affinity-session",
			},
			want: "opencode-session",
		},
		{
			name: "opencode headers beat ambiguous client metadata",
			headers: map[string]string{
				"x-session-id":       "opencode-session",
				"x-session-affinity": "opencode-session",
			},
			body: `{"client_metadata":{"session_id":"unrelated-session"}}`,
			want: "opencode-session",
		},
		{
			name: "opencode openai oauth",
			headers: map[string]string{
				"originator":         "opencode",
				"session-id":         "oauth-session",
				"x-session-id":       "opencode-session",
				"x-session-affinity": "opencode-session",
			},
			want: "opencode-session",
		},
		{
			name: "pi codex adapter prefers hyphenated header",
			headers: map[string]string{
				"originator":          "pi",
				"session-id":          "pi-codex-session",
				"session_id":          "other-session",
				"x-client-request-id": "pi-codex-session",
			},
			want: "pi-codex-session",
		},
		{
			name: "pi openai responses",
			headers: map[string]string{
				"session_id":          "pi-response-session",
				"x-client-request-id": "pi-response-session",
			},
			want: "pi-response-session",
		},
		{
			name:    "pi openrouter",
			headers: map[string]string{"x-session-id": "pi-openrouter-session"},
			want:    "pi-openrouter-session",
		},
		{
			name:    "pi no-session cache format",
			headers: map[string]string{"x-client-request-id": "pi-cache-session"},
			body:    `{"prompt_cache_key":"pi-cache-session"}`,
			want:    "pi-cache-session",
		},
		{
			name:    "pi identity allows cache key fallback",
			headers: map[string]string{"originator": "pi"},
			body:    `{"prompt_cache_key":"pi-cache-session"}`,
			want:    "pi-cache-session",
		},
		{
			name: "pi messages options",
			body: `{"options":{"sessionId":"pi-message-session"}}`,
			want: "pi-message-session",
		},
		{
			name:    "request id alone is not a session",
			headers: map[string]string{"x-client-request-id": "request-only"},
		},
		{
			name: "cache key alone is not a session",
			body: `{"prompt_cache_key":"cache-only"}`,
		},
		{
			name:    "empty values are ignored",
			headers: map[string]string{"session-id": "   "},
		},
		{
			name:    "oversized value is ignored",
			headers: map[string]string{"session-id": strings.Repeat("x", maxSessionIDLength+1)},
		},
		{name: "no session metadata"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			for key, value := range tc.headers {
				header.Set(key, value)
			}
			if got := extractAgentSessionID(header, []byte(tc.body)); got != tc.want {
				t.Errorf("extractAgentSessionID() = %q, want %q", got, tc.want)
			}
		})
	}
}
