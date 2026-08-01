package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abowloflrf/apid/config"
	"github.com/abowloflrf/apid/store"
)

func ptr(s string) *string { return &s }

func topologyConfig() config.Config {
	return config.Config{
		Listen: ":19092",
		Upstreams: []config.Upstream{
			{Name: "deepseek", Protocol: config.ProtoChat, BaseURL: "https://api.deepseek.com/v1", Path: "/chat/completions", APIKey: "sk-secret-chat", Model: "deepseek-chat"},
			{Name: "anthropic", Protocol: config.ProtoAnthropic, BaseURL: "https://api.anthropic.com", Path: "/v1/messages", APIKey: "sk-secret-ant"},
		},
		Routes: []config.Route{
			{Path: "/v1/responses", InputProtocol: config.ProtoResponses, Models: []config.ModelRule{
				{Match: "gpt-4o", Upstream: "deepseek", Model: ptr("deepseek-reasoner")},
				{Match: "ds-*", Upstream: "deepseek"},
				{Match: "*", Upstream: "deepseek", Model: ptr("")},
			}},
			{Path: "/v1/messages", InputProtocol: config.ProtoAnthropic, Models: []config.ModelRule{
				{Match: "claude-*", Upstream: "anthropic"},
			}},
		},
	}
}

func TestTopology(t *testing.T) {
	top := New(topologyConfig(), &store.Store{}, nil).topology()

	if len(top.Routes) != 2 || len(top.Upstreams) != 2 {
		t.Fatalf("got %d routes / %d upstreams, want 2 / 2", len(top.Routes), len(top.Upstreams))
	}
	if top.Storage || top.ClientAuth || top.Search != nil {
		t.Errorf("storage/auth/search should be off: %+v", top)
	}

	rules := top.Routes[0].Rules
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
	// responses -> chat is a conversion; the per-rule model wins over the upstream's.
	if rules[0].Mode != "convert" || rules[0].MatchKind != "exact" ||
		rules[0].ModelSource != "rule" || rules[0].EffectiveModel != "deepseek-reasoner" {
		t.Errorf("exact rule = %+v", rules[0])
	}
	// Key omitted => inherit the upstream's model.
	if rules[1].MatchKind != "glob" || rules[1].ModelSource != "upstream" || rules[1].EffectiveModel != "deepseek-chat" {
		t.Errorf("glob rule = %+v", rules[1])
	}
	// model = "" forces the client's model through even though the upstream sets one.
	if rules[2].MatchKind != "catchall" || rules[2].ModelSource != "passthrough" || rules[2].EffectiveModel != "" {
		t.Errorf("catch-all rule = %+v", rules[2])
	}
	if r := top.Routes[1].Rules[0]; r.Mode != "forward" {
		t.Errorf("same-protocol route mode = %q, want forward", r.Mode)
	}

	ds := top.Upstreams[0]
	if ds.RefCount != 3 || ds.Endpoint != "https://api.deepseek.com/v1/chat/completions" {
		t.Errorf("deepseek upstream = %+v", ds)
	}
	if ds.Auth != "api_key" || ds.AuthHdr != "Authorization: Bearer" {
		t.Errorf("chat auth = %q / %q", ds.Auth, ds.AuthHdr)
	}
	if ant := top.Upstreams[1]; ant.AuthHdr != "X-Api-Key" {
		t.Errorf("anthropic auth header = %q, want X-Api-Key", ant.AuthHdr)
	}
}

// TestTopologyDualProtocol: supports_responses 的 upstream 在 responses 入口下
// 显示为 raw forward（via_responses），端点换成 responses 端点。
func TestTopologyDualProtocol(t *testing.T) {
	cfg := config.Config{
		Upstreams: []config.Upstream{{
			Name:              "up",
			Protocol:          config.ProtoChat,
			BaseURL:           "https://api.example.com",
			Path:              "/v1/chat/completions",
			SupportsResponses: true,
		}},
		Routes: []config.Route{{
			Path:          "/v1/responses",
			InputProtocol: config.ProtoResponses,
			Models:        []config.ModelRule{{Upstream: "up"}},
		}},
	}
	top := New(cfg, &store.Store{}, nil).topology()

	r := top.Routes[0].Rules[0]
	if r.Mode != "forward" || !r.ViaResponses || r.UpstreamProto != string(config.ProtoResponses) {
		t.Errorf("rule = %+v, want forward via responses with wire protocol openai_responses", r)
	}
	if r.Endpoint != "https://api.example.com/v1/responses" {
		t.Errorf("rule endpoint = %q, want responses endpoint", r.Endpoint)
	}

	u := top.Upstreams[0]
	if !u.SupportsResponses || u.ResponsesEndpoint != "https://api.example.com/v1/responses" {
		t.Errorf("upstream = %+v, want supports_responses with responses endpoint", u)
	}
}

// Upstream keys must never reach the browser; only the mode they imply.
func TestTopologyHidesSecrets(t *testing.T) {
	cfg := topologyConfig()
	cfg.ClientAPIKey = "apid-inbound-key"
	cfg.Search = config.SearchConfig{Provider: "exa", APIKey: "exa-secret"}

	w := httptest.NewRecorder()
	New(cfg, &store.Store{}, nil).Handler().ServeHTTP(w, httptest.NewRequest("GET", "/stats/topology", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, secret := range []string{"sk-secret-chat", "sk-secret-ant", "apid-inbound-key", "exa-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q: %s", secret, body)
		}
	}

	var top topologyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !top.ClientAuth || top.Search == nil || top.Search.Path != "/v1/alpha/search" {
		t.Errorf("client auth / search summary = %+v, %+v", top.ClientAuth, top.Search)
	}
}

// Client auth strips inbound credentials before forwarding, so an upstream with
// no key of its own ends up unauthenticated — the UI needs to be able to say so.
func TestTopologyAuthStripped(t *testing.T) {
	cfg := config.Config{
		ClientAPIKey: "inbound",
		Upstreams:    []config.Upstream{{Name: "u", Protocol: config.ProtoChat, BaseURL: "https://x", Path: "/v1/chat/completions"}},
		Routes:       []config.Route{{Path: "/v1/chat/completions", InputProtocol: config.ProtoChat, Models: []config.ModelRule{{Upstream: "u"}}}},
	}
	if got := New(cfg, &store.Store{}, nil).topology().Upstreams[0].Auth; got != "stripped" {
		t.Errorf("auth = %q, want stripped", got)
	}
	cfg.ClientAPIKey = ""
	if got := New(cfg, &store.Store{}, nil).topology().Upstreams[0].Auth; got != "passthrough" {
		t.Errorf("auth = %q, want passthrough", got)
	}
}

// The config view reads config, not the DB, so it stays available with storage
// off — unlike every other /stats/* endpoint.
func TestTopologyWorksWithoutStorage(t *testing.T) {
	w := httptest.NewRecorder()
	New(topologyConfig(), &store.Store{}, nil).Handler().ServeHTTP(w, httptest.NewRequest("GET", "/stats/topology", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
