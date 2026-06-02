package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTOML writes a TOML file in a temp dir and returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFileValid(t *testing.T) {
	path := writeTOML(t, `
[[upstream]]
name = "deepseek"
protocol = "openai_chat_completions"
base_url = "https://api.deepseek.com/v1"
path = "/chat/completions"
api_key = "sk-xxx"
model = "deepseek-chat"

[[upstream]]
name = "openai"
protocol = "openai_chat_completions"
base_url = "https://api.openai.com"
path = "/v1/chat/completions"

# convert entrypoint: Responses in, Chat upstreams out
[[route]]
path = "/v1/responses"
input_protocol = "openai_responses"
  [[route.model]]
  match = "deepseek-*"
  upstream = "deepseek"
  [[route.model]]
  match = "*"
  upstream = "openai"

# pure-forward entrypoint reusing the same upstreams
[[route]]
path = "/v1/chat/completions"
input_protocol = "openai_chat_completions"
  [[route.model]]
  upstream = "openai"
`)
	ups, routes, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile failed: %v", err)
	}
	if len(ups) != 2 || len(routes) != 2 {
		t.Fatalf("got %d upstreams, %d routes; want 2/2", len(ups), len(routes))
	}
	if ups[0].Model != "deepseek-chat" {
		t.Errorf("upstream[0].Model = %q", ups[0].Model)
	}
	if len(routes[0].Models) != 2 {
		t.Errorf("route[0] should have 2 model rules, got %d", len(routes[0].Models))
	}
}

// TestUpstreamFor checks match priority: exact > glob > catch-all.
func TestUpstreamFor(t *testing.T) {
	r := Route{Models: []ModelRule{
		{Match: "gpt-4o", Upstream: "exact"},
		{Match: "claude-*", Upstream: "glob"},
		{Match: "*", Upstream: "fallback"},
	}}
	cases := []struct{ model, want string }{
		{"gpt-4o", "exact"},
		{"claude-3-5-sonnet", "glob"},
		{"deepseek-chat", "fallback"},
		{"", "fallback"},
	}
	for _, tc := range cases {
		got, ok := r.UpstreamFor(tc.model)
		if !ok || got != tc.want {
			t.Errorf("UpstreamFor(%q) = %q,%v; want %q", tc.model, got, ok, tc.want)
		}
	}

	// No catch-all => unmatched model is unroutable.
	r2 := Route{Models: []ModelRule{{Match: "gpt-4o", Upstream: "x"}}}
	if _, ok := r2.UpstreamFor("other"); ok {
		t.Errorf("UpstreamFor with no catch-all should miss")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"claude-*", "claude-3", true},
		{"claude-*", "gpt-4", false},
		{"*-turbo", "gpt-4-turbo", true},
		{"a*b*c", "axxbyyc", true},
		{"vendor/*", "vendor/model", true},
		{"exact", "exact", true},
		{"exact", "other", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q,%q) = %v; want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestLoadFileErrors(t *testing.T) {
	cases := []struct {
		name, toml, wantSubstr string
	}{
		{
			name:       "no upstream",
			toml:       "[[route]]\npath=\"/x\"\ninput_protocol=\"openai_responses\"\n[[route.model]]\nupstream=\"u\"",
			wantSubstr: "[[upstream]]",
		},
		{
			name: "duplicate upstream name",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`,
			wantSubstr: "duplicate upstream",
		},
		{
			name: "upstream invalid protocol",
			toml: `
[[upstream]]
name = "u"
protocol = "bogus"
base_url = "b"
path = "/p"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`,
			wantSubstr: "invalid protocol",
		},
		{
			name: "route references unknown upstream",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "nope"`,
			wantSubstr: "undefined upstream",
		},
		{
			name: "reverse conversion unsupported",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_responses"
base_url = "b"
path = "/p"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`,
			wantSubstr: "not supported",
		},
		{
			name: "path missing leading slash",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
[[route]]
path = "v1/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`,
			wantSubstr: "must start with /",
		},
		{
			name: "duplicate route path",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`,
			wantSubstr: "duplicate route path",
		},
		{
			name: "route with no model rule",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"`,
			wantSubstr: "at least one [[route.model]]",
		},
		{
			name: "multiple catch-all",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
match = "*"
upstream = "u"
[[route.model]]
upstream = "u"`,
			wantSubstr: "catch-all",
		},
		{
			name: "unknown field",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/p"
bogus = "oops"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`,
			wantSubstr: "unknown keys",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTOML(t, tc.toml)
			_, _, err := loadFile(path)
			if err == nil {
				t.Fatalf("expected error, got success")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestLoadFileMissing(t *testing.T) {
	_, _, err := loadFile(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err == nil {
		t.Fatal("missing file should error")
	}
}
