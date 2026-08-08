package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
client_api_key = "apid-client-key"

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

	fc, err := loadFullFile(path)
	if err != nil {
		t.Fatalf("loadFullFile failed: %v", err)
	}
	if fc.ClientAPIKey != "apid-client-key" {
		t.Errorf("ClientAPIKey = %q, want apid-client-key", fc.ClientAPIKey)
	}
}

func TestLoadFileValidAnthropicMessages(t *testing.T) {
	path := writeTOML(t, `
[[upstream]]
name = "anthropic"
protocol = "anthropic_messages"
base_url = "https://api.anthropic.com"
path = "/v1/messages"
api_key = "sk-ant-xxx"

[[route]]
path = "/v1/messages"
input_protocol = "anthropic_messages"
  [[route.model]]
  match = "claude-*"
  upstream = "anthropic"
`)
	ups, routes, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile failed: %v", err)
	}
	if ups[0].Protocol != ProtoAnthropic || routes[0].InputProtocol != ProtoAnthropic {
		t.Errorf("protocol not loaded as anthropic: upstream=%q route=%q", ups[0].Protocol, routes[0].InputProtocol)
	}
}

// TestLoadFileValidDualProtocol: 一个同时支持 Chat 与 Responses 的供应商只写一份
// upstream，responses_path 可从 path 推导。
func TestLoadFileValidDualProtocol(t *testing.T) {
	path := writeTOML(t, `
[[upstream]]
name = "openai"
protocol = "openai_chat_completions"
base_url = "https://api.openai.com"
path = "/v1/chat/completions"
api_key = "sk-xxx"
supports_responses = true

[[upstream]]
name = "openai-custom"
protocol = "openai_chat_completions"
base_url = "https://api.example.com"
path = "/v1/chat/completions"
supports_responses = true
responses_path = "/custom/responses"

[[route]]
path = "/v1/responses"
input_protocol = "openai_responses"
  [[route.model]]
  upstream = "openai"
  [[route.model]]
  match = "custom-*"
  upstream = "openai-custom"
`)
	ups, _, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile failed: %v", err)
	}
	if !ups[0].SupportsResponses {
		t.Errorf("upstream[0].SupportsResponses = false, want true")
	}
	if got := ups[0].EffectiveResponsesPath(); got != "/v1/responses" {
		t.Errorf("EffectiveResponsesPath() = %q, want /v1/responses", got)
	}
	if !ups[1].SupportsResponses {
		t.Errorf("upstream[1].SupportsResponses = false, want true")
	}
	if got := ups[1].EffectiveResponsesPath(); got != "/custom/responses" {
		t.Errorf("EffectiveResponsesPath() = %q, want /custom/responses", got)
	}
}

func TestLoadFileRejectsAnthropicConversion(t *testing.T) {
	path := writeTOML(t, `
[[upstream]]
name = "anthropic"
protocol = "anthropic_messages"
base_url = "https://api.anthropic.com"
path = "/v1/messages"

[[route]]
path = "/v1/responses"
input_protocol = "openai_responses"
  [[route.model]]
  upstream = "anthropic"
`)
	_, _, err := loadFile(path)
	if err == nil {
		t.Fatal("expected unsupported conversion error, got success")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error = %q, want not supported", err.Error())
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

// TestResolveModelOverride checks Resolve returns the matched rule, carrying its
// per-rule Model override (nil / "" / value are distinct).
func TestResolveModelOverride(t *testing.T) {
	rewrite := "deepseek-reasoner"
	passthrough := ""
	r := Route{Models: []ModelRule{
		{Match: "gpt-4o", Upstream: "ds", Model: &rewrite},
		{Match: "keep-*", Upstream: "ds", Model: &passthrough},
		{Match: "*", Upstream: "ds"}, // nil = inherit upstream
	}}

	m, ok := r.Resolve("gpt-4o")
	if !ok || m.Model == nil || *m.Model != rewrite {
		t.Errorf("Resolve(gpt-4o) model = %v; want %q", m.Model, rewrite)
	}
	m, ok = r.Resolve("keep-this")
	if !ok || m.Model == nil || *m.Model != "" {
		t.Errorf("Resolve(keep-this) model = %v; want explicit empty", m.Model)
	}
	m, ok = r.Resolve("other")
	if !ok || m.Model != nil {
		t.Errorf("Resolve(other) model = %v; want nil (inherit)", m.Model)
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
		{
			name: "supports_responses on non-chat protocol",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_responses"
base_url = "b"
path = "/v1/responses"
supports_responses = true
[[route]]
path = "/x"
input_protocol = "openai_responses"
[[route.model]]
upstream = "u"`,
			wantSubstr: "supports_responses only applies to openai_chat_completions",
		},
		{
			name: "responses_path without supports_responses",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/v1/chat/completions"
responses_path = "/v1/responses"
[[route]]
path = "/x"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`,
			wantSubstr: "responses_path requires supports_responses",
		},
		{
			name: "supports_responses without derivable path",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/v1/completions"
supports_responses = true
[[route]]
path = "/x"
input_protocol = "openai_responses"
[[route.model]]
upstream = "u"`,
			wantSubstr: "responses_path",
		},
		{
			name: "responses_path missing leading slash",
			toml: `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "b"
path = "/v1/chat/completions"
supports_responses = true
responses_path = "v1/responses"
[[route]]
path = "/x"
input_protocol = "openai_responses"
[[route.model]]
upstream = "u"`,
			wantSubstr: "must start with /",
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

// minimalTOML is the smallest config that passes validation, used by Load tests
// that only care about the env-derived fields.
const minimalTOML = `
[[upstream]]
name = "u"
protocol = "openai_chat_completions"
base_url = "https://api.example.com"
path = "/v1/chat/completions"
[[route]]
path = "/v1/chat/completions"
input_protocol = "openai_chat_completions"
[[route.model]]
upstream = "u"`

// isolateEnv points APID_ENV_FILE at a non-existent path so Load won't pick up a
// real .env in the working dir, and clears the vars Load reads so the host's
// environment can't leak into assertions.
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("APID_ENV_FILE", filepath.Join(t.TempDir(), "no.env"))
	t.Setenv("APID_LISTEN", "")
	t.Setenv("APID_DB", "")
	t.Setenv("APID_TRACE", "")
	t.Setenv("APID_TRACE_DIR", "")
	t.Setenv("APID_SHUTDOWN_TIMEOUT", "")
	t.Setenv("APID_CODEX_SSE_IDLE_TIMEOUT", "")
}

func TestLoadDefaults(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(writeTOML(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":19092" {
		t.Errorf("Listen = %q, want default :19092", cfg.Listen)
	}
	if cfg.DB != "" || cfg.TraceDir != "" {
		t.Errorf("DB=%q TraceDir=%q, want both empty", cfg.DB, cfg.TraceDir)
	}
	if cfg.ShutdownTimeout != 120*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 120s", cfg.ShutdownTimeout)
	}
	if cfg.CodexSSEIdleTimeout != 5*time.Minute {
		t.Errorf("CodexSSEIdleTimeout = %s, want 5m", cfg.CodexSSEIdleTimeout)
	}
	if cfg.ClientAPIKey != "" {
		t.Errorf("ClientAPIKey = %q, want empty", cfg.ClientAPIKey)
	}
	if len(cfg.Upstreams) != 1 || len(cfg.Routes) != 1 {
		t.Errorf("got %d upstreams, %d routes; want 1/1", len(cfg.Upstreams), len(cfg.Routes))
	}
}

func TestLoadClientAPIKeyFromConfig(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(writeTOML(t, `client_api_key = "local-key"`+minimalTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClientAPIKey != "local-key" {
		t.Errorf("ClientAPIKey = %q, want local-key", cfg.ClientAPIKey)
	}
}

func TestLoadStatsAPIKeyFromConfig(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(writeTOML(t, `stats_api_key = "stats-view-key"`+minimalTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StatsAPIKey != "stats-view-key" {
		t.Errorf("StatsAPIKey = %q, want stats-view-key", cfg.StatsAPIKey)
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	isolateEnv(t)
	t.Setenv("APID_LISTEN", ":8080")
	t.Setenv("APID_DB", "/tmp/apid.db")
	t.Setenv("APID_TRACE_DIR", "/var/trace")

	cfg, err := Load(writeTOML(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want :8080", cfg.Listen)
	}
	if cfg.DB != "/tmp/apid.db" {
		t.Errorf("DB = %q, want /tmp/apid.db", cfg.DB)
	}
	if cfg.TraceDir != "/var/trace" {
		t.Errorf("TraceDir = %q, want /var/trace", cfg.TraceDir)
	}
}

// APID_TRACE=1 with no explicit dir defaults the trace dir to ./logs.
func TestLoadTraceDefaultDir(t *testing.T) {
	isolateEnv(t)
	t.Setenv("APID_TRACE", "1")

	cfg, err := Load(writeTOML(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TraceDir != "./logs" {
		t.Errorf("TraceDir = %q, want ./logs", cfg.TraceDir)
	}
}

func TestLoadInvalidConfig(t *testing.T) {
	isolateEnv(t)
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("Load with missing config should error")
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		if truthy(v) {
			t.Errorf("truthy(%q) = true, want false", v)
		}
	}
}

func TestLoadCodexSSEIdleTimeout(t *testing.T) {
	isolateEnv(t)
	t.Setenv("APID_CODEX_SSE_IDLE_TIMEOUT", "45s")
	cfg, err := Load(writeTOML(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CodexSSEIdleTimeout != 45*time.Second {
		t.Fatalf("CodexSSEIdleTimeout = %s", cfg.CodexSSEIdleTimeout)
	}

	t.Setenv("APID_CODEX_SSE_IDLE_TIMEOUT", "invalid")
	if _, err := Load(writeTOML(t, minimalTOML)); err == nil {
		t.Fatal("invalid APID_CODEX_SSE_IDLE_TIMEOUT should fail")
	}
}

func TestLoadShutdownTimeout(t *testing.T) {
	isolateEnv(t)
	t.Setenv("APID_SHUTDOWN_TIMEOUT", "45s")
	cfg, err := Load(writeTOML(t, minimalTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ShutdownTimeout != 45*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 45s", cfg.ShutdownTimeout)
	}

	for _, value := range []string{"invalid", "0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("APID_SHUTDOWN_TIMEOUT", value)
			if _, err := Load(writeTOML(t, minimalTOML)); err == nil {
				t.Fatalf("APID_SHUTDOWN_TIMEOUT=%q should fail", value)
			}
		})
	}
}

const subscriptionTOML = `
client_api_key = "local-key"

[[upstream]]
name = "codex-subscription"
protocol = "openai_responses"
base_url = "https://chatgpt.com/backend-api/codex"
path = "/responses"
auth_mode = "codex_subscription"

[[route]]
path = "/codex/v1/responses"
input_protocol = "openai_responses"
  [[route.model]]
  match = "*"
  upstream = "codex-subscription"
  model = ""

[[route]]
path = "/codex/v1/responses/compact"
input_protocol = "openai_responses"
operation = "responses_compact"
  [[route.model]]
  match = "*"
  upstream = "codex-subscription"
  model = ""
`

func TestLoadCodexSubscription(t *testing.T) {
	isolateEnv(t)
	t.Setenv("APID_LISTEN", "127.0.0.1:19092")
	cfg, err := Load(writeTOML(t, subscriptionTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Upstreams[0].AuthMode != AuthModeCodexSubscription {
		t.Fatalf("AuthMode = %q", cfg.Upstreams[0].AuthMode)
	}
	if cfg.Routes[1].Operation != RouteOperationResponsesCompact {
		t.Fatalf("Operation = %q", cfg.Routes[1].Operation)
	}
}

func TestLoadCodexSubscriptionRequiresLoopback(t *testing.T) {
	for _, tc := range []struct {
		listen string
		ok     bool
	}{
		{"127.0.0.1:19092", true},
		{"127.42.0.9:19092", true},
		{"[::1]:19092", true},
		{"localhost:19092", false},
		{":19092", false},
		{"0.0.0.0:19092", false},
		{"192.168.1.2:19092", false},
		{"[fe80::1%lo]:19092", false},
		{"[::ffff:127.0.0.1]:19092", false},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("APID_LISTEN", tc.listen)
			_, err := Load(writeTOML(t, subscriptionTOML))
			if (err == nil) != tc.ok {
				t.Fatalf("Load error = %v, want success=%v", err, tc.ok)
			}
		})
	}
}

func TestLoadFileRejectsUnsafeCodexSubscriptionConfig(t *testing.T) {
	cases := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{"unknown auth mode", `auth_mode = "codex_subscription"`, `auth_mode = "passthrough"`, "invalid auth_mode"},
		{"wrong origin", CodexSubscriptionBaseURL, "https://example.com/backend-api/codex", "base_url must be"},
		{"userinfo origin", CodexSubscriptionBaseURL, "https://user@chatgpt.com/backend-api/codex", "base_url must be"},
		{"query in origin", CodexSubscriptionBaseURL, CodexSubscriptionBaseURL + "?target=evil", "base_url must be"},
		{"fragment in origin", CodexSubscriptionBaseURL, CodexSubscriptionBaseURL + "#fragment", "base_url must be"},
		{"wrong path", `path = "/responses"`, `path = "/other"`, "path must be"},
		{"wrong protocol", `protocol = "openai_responses"`, `protocol = "openai_chat_completions"`, "protocol must be"},
		{"api key", `auth_mode = "codex_subscription"`, "api_key = \"secret\"\nauth_mode = \"codex_subscription\"", "api_key must be empty"},
		{"upstream model", `auth_mode = "codex_subscription"`, "model = \"gpt-x\"\nauth_mode = \"codex_subscription\"", "model must be empty"},
		{"rule model omitted", `  model = ""`, ``, `requires model = ""`},
		{"implicit catchall", `  match = "*"`, ``, `exactly one match = "*"`},
		{"extra exact rule", `  model = ""`, "  model = \"\"\n  [[route.model]]\n  match = \"gpt-*\"\n  upstream = \"codex-subscription\"\n  model = \"\"", "exactly one match"},
		{"unknown operation", `operation = "responses_compact"`, `operation = "arbitrary_proxy"`, "invalid operation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contents := strings.Replace(subscriptionTOML, tc.old, tc.new, 1)
			_, _, err := loadFile(writeTOML(t, contents))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadFileRejectsMixedCodexSubscriptionRoute(t *testing.T) {
	path := writeTOML(t, `
[[upstream]]
name = "subscription"
protocol = "openai_responses"
base_url = "https://chatgpt.com/backend-api/codex"
path = "/responses"
auth_mode = "codex_subscription"

[[upstream]]
name = "normal"
protocol = "openai_responses"
base_url = "https://api.example.com"
path = "/v1/responses"

[[route]]
path = "/codex/v1/responses"
input_protocol = "openai_responses"
  [[route.model]]
  match = "gpt-*"
  upstream = "normal"
  model = ""
  [[route.model]]
  match = "*"
  upstream = "subscription"
  model = ""
`)
	_, _, err := loadFile(path)
	if err == nil || !strings.Contains(err.Error(), "must not mix") {
		t.Fatalf("error = %v, want mixed auth-mode rejection", err)
	}
}
