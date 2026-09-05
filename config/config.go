// Package config loads runtime configuration: ops params (listen addr, SQLite,
// TRACE) from env vars; forwarding config from a TOML file with two tables:
//   - [[upstream]]: a backend deployment (protocol/addr/auth/model), defined once
//     and referenced by name.
//   - [[route]]: a public entrypoint (path + client protocol) that dispatches by
//     the request's model to one upstream.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/joho/godotenv"
)

// Protocol is the API protocol spoken on one end.
type Protocol string

const (
	ProtoResponses Protocol = "openai_responses"
	ProtoChat      Protocol = "openai_chat_completions"
	ProtoAnthropic Protocol = "anthropic_messages"
)

// AuthMode selects the credential policy for one upstream. The zero value
// keeps the existing API-key behavior.
type AuthMode string

const (
	AuthModeDefault           AuthMode = ""
	AuthModeCodexSubscription AuthMode = "codex_subscription"
)

// RouteOperation selects one of the narrowly supported Responses operations.
type RouteOperation string

const (
	RouteOperationInference        RouteOperation = ""
	RouteOperationResponsesCompact RouteOperation = "responses_compact"
)

const (
	CodexSubscriptionBaseURL = "https://chatgpt.com/backend-api/codex"
	CodexSubscriptionPath    = "/responses"
	DefaultMaxRequestBody    = int64(64 * 1024 * 1024)
)

type Config struct {
	Listen              string        // APID_LISTEN
	TraceDir            string        // APID_TRACE_DIR / APID_TRACE; empty = off
	DB                  string        // APID_DB; empty = off
	ShutdownTimeout     time.Duration // APID_SHUTDOWN_TIMEOUT; defaults to 120s
	MaxRequestBody      int64         // APID_MAX_REQUEST_BODY; bytes, defaults to 64 MiB
	ClientAPIKey        string        // client_api_key in TOML; empty = no inbound client auth
	StatsAPIKey         string        // stats_api_key in TOML; empty = /stats(*) dashboard stays open
	CodexSSEIdleTimeout time.Duration // APID_CODEX_SSE_IDLE_TIMEOUT; defaults to 5m
	Search              SearchConfig  // optional; zero value = search endpoint disabled
	Upstreams           []Upstream
	Routes              []Route
}

// Upstream is a backend deployment, referenced by Name from any number of routes
// so its address/auth/model are written once, not per entrypoint.
type Upstream struct {
	Name              string   `toml:"name"`
	Protocol          Protocol `toml:"protocol"`
	BaseURL           string   `toml:"base_url"`
	Path              string   `toml:"path"`
	APIKey            string   `toml:"api_key"`            // empty = pass through client auth unless client_api_key is enabled
	Model             string   `toml:"model"`              // empty = pass through client model
	SupportsResponses bool     `toml:"supports_responses"` // backend also speaks OpenAI Responses natively
	ResponsesPath     string   `toml:"responses_path"`     // optional; empty = derived from Path
	AuthMode          AuthMode `toml:"auth_mode"`          // empty = normal API key; codex_subscription = fixed credential passthrough
}

// EffectiveResponsesPath returns the OpenAI Responses endpoint path actually
// used when SupportsResponses is set: an explicit responses_path wins, otherwise
// Path's trailing "/chat/completions" is swapped for "/responses"
// ("/v1/chat/completions" -> "/v1/responses", "/chat/completions" -> "/responses").
// Validation guarantees the result is non-empty for dual-protocol upstreams.
func (u Upstream) EffectiveResponsesPath() string {
	if u.ResponsesPath != "" {
		return u.ResponsesPath
	}
	return strings.TrimSuffix(u.Path, "/chat/completions") + "/responses"
}

// ModelRule dispatches a route by model. Match is an exact name, a glob
// ("claude-*"), or "" / "*" as the catch-all. Model overrides the forwarded
// model id for this rule: nil (key omitted) inherits the upstream's model;
// a non-nil value wins, where "" forces passing the client's model through and
// a non-empty value rewrites it. This lets rules sharing one upstream pick
// different rewrite strategies.
type ModelRule struct {
	Match    string  `toml:"match"`
	Upstream string  `toml:"upstream"`
	Model    *string `toml:"model"`
}

// Route is a public entrypoint: a unique Path and the client-side InputProtocol.
// Whether conversion happens depends on InputProtocol vs the matched upstream's
// Protocol.
type Route struct {
	Path          string         `toml:"path"`
	InputProtocol Protocol       `toml:"input_protocol"`
	Operation     RouteOperation `toml:"operation"`
	Models        []ModelRule    `toml:"model"`
}

// SearchConfig configures the optional standalone web-search endpoint
// (POST /v1/alpha/search) that Codex invokes when the model provider opts into
// supports_standalone_web_search. Currently only Exa is supported as a backend.
type SearchConfig struct {
	Provider string `toml:"provider"` // "exa"; empty = search endpoint disabled
	APIKey   string `toml:"api_key"`  // Exa API key
	BaseURL  string `toml:"base_url"` // optional; defaults to "https://api.exa.ai"
	Path     string `toml:"path"`     // optional; defaults to "/v1/alpha/search"
}

// Resolve picks the matching model rule for a model: exact > glob > catch-all.
// Among multiple matching globs the first in config order wins.
func (r Route) Resolve(model string) (ModelRule, bool) {
	for _, m := range r.Models { // exact
		if m.Match != "" && !isWildcard(m.Match) && m.Match == model {
			return m, true
		}
	}
	for _, m := range r.Models { // glob
		if m.Match != "*" && isWildcard(m.Match) && globMatch(m.Match, model) {
			return m, true
		}
	}
	for _, m := range r.Models { // catch-all
		if m.Match == "" || m.Match == "*" {
			return m, true
		}
	}
	return ModelRule{}, false
}

// UpstreamFor picks the upstream name for a model via Resolve.
func (r Route) UpstreamFor(model string) (string, bool) {
	if m, ok := r.Resolve(model); ok {
		return m.Upstream, true
	}
	return "", false
}

func isWildcard(s string) bool { return strings.Contains(s, "*") }

// globMatch matches with "*" as any-length run. Unlike path.Match it does not
// treat "/" specially, so "vendor/model" names work.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		idx := strings.Index(s, p)
		if idx < 0 {
			return false
		}
		s = s[idx+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

type fileConfig struct {
	ClientAPIKey string       `toml:"client_api_key"`
	StatsAPIKey  string       `toml:"stats_api_key"`
	Search       SearchConfig `toml:"search"`
	Upstreams    []Upstream   `toml:"upstream"`
	Routes       []Route      `toml:"route"`
}

// Load reads ops params from env and upstreams/routes from the TOML file at
// configPath (the path comes from the --config flag). A .env is loaded first
// without overriding real env vars.
func Load(configPath string) (Config, error) {
	envFile := os.Getenv("APID_ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	_ = godotenv.Load(envFile)

	traceDir := os.Getenv("APID_TRACE_DIR")
	if traceDir == "" && truthy(os.Getenv("APID_TRACE")) {
		traceDir = "./logs"
	}

	fc, err := loadFullFile(configPath)
	if err != nil {
		return Config{}, err
	}
	codexSSEIdleTimeout, err := durationEnv("APID_CODEX_SSE_IDLE_TIMEOUT", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := durationEnv("APID_SHUTDOWN_TIMEOUT", 120*time.Second)
	if err != nil {
		return Config{}, err
	}
	maxRequestBody, err := positiveInt64Env("APID_MAX_REQUEST_BODY", DefaultMaxRequestBody)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:              env("APID_LISTEN", ":19092"),
		TraceDir:            traceDir,
		DB:                  env("APID_DB", ""),
		ShutdownTimeout:     shutdownTimeout,
		MaxRequestBody:      maxRequestBody,
		ClientAPIKey:        fc.ClientAPIKey,
		StatsAPIKey:         fc.StatsAPIKey,
		CodexSSEIdleTimeout: codexSSEIdleTimeout,
		Search:              fc.Search,
		Upstreams:           fc.Upstreams,
		Routes:              fc.Routes,
	}
	if err := validateRuntimeConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadFile(path string) ([]Upstream, []Route, error) {
	fc, err := loadFullFile(path)
	if err != nil {
		return nil, nil, err
	}
	return fc.Upstreams, fc.Routes, nil
}

func loadFullFile(path string) (fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, fmt.Errorf("config: read %q failed (set --config): %w", path, err)
	}
	var fc fileConfig
	md, err := toml.Decode(string(data), &fc)
	if err != nil {
		return fileConfig{}, fmt.Errorf("config: parse %q failed: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return fileConfig{}, fmt.Errorf("config: %q has unknown keys: %v", path, undec)
	}
	if err := validateSearchConfig(fc.Search); err != nil {
		return fileConfig{}, err
	}
	if err := validateConfig(fc.Upstreams, fc.Routes); err != nil {
		return fileConfig{}, err
	}
	return fc, nil
}

func validateConfig(upstreams []Upstream, routes []Route) error {
	if len(upstreams) == 0 {
		return fmt.Errorf("config: need at least one [[upstream]]")
	}
	byName := make(map[string]Upstream, len(upstreams))
	for _, u := range upstreams {
		if u.Name == "" {
			return fmt.Errorf("config: upstream name must not be empty")
		}
		if _, dup := byName[u.Name]; dup {
			return fmt.Errorf("config: duplicate upstream name %q", u.Name)
		}
		if !isValidProtocol(u.Protocol) {
			return fmt.Errorf("config: upstream %q has invalid protocol %q", u.Name, u.Protocol)
		}
		if !isValidAuthMode(u.AuthMode) {
			return fmt.Errorf("config: upstream %q has invalid auth_mode %q", u.Name, u.AuthMode)
		}
		if u.BaseURL == "" {
			return fmt.Errorf("config: upstream %q base_url must not be empty", u.Name)
		}
		if u.Path == "" {
			return fmt.Errorf("config: upstream %q path must not be empty", u.Name)
		}
		if u.SupportsResponses && u.Protocol != ProtoChat {
			return fmt.Errorf("config: upstream %q supports_responses only applies to openai_chat_completions (protocol is %q)", u.Name, u.Protocol)
		}
		if u.ResponsesPath != "" && !u.SupportsResponses {
			return fmt.Errorf("config: upstream %q responses_path requires supports_responses = true", u.Name)
		}
		if u.SupportsResponses && u.ResponsesPath == "" && !strings.HasSuffix(u.Path, "/chat/completions") {
			return fmt.Errorf("config: upstream %q supports_responses needs an explicit responses_path (cannot derive it from path %q)", u.Name, u.Path)
		}
		if u.ResponsesPath != "" && !strings.HasPrefix(u.ResponsesPath, "/") {
			return fmt.Errorf("config: upstream %q responses_path %q must start with /", u.Name, u.ResponsesPath)
		}
		if u.AuthMode == AuthModeCodexSubscription {
			if err := validateCodexSubscriptionUpstream(u); err != nil {
				return err
			}
		}
		byName[u.Name] = u
	}

	if len(routes) == 0 {
		return fmt.Errorf("config: need at least one [[route]]")
	}
	seenPath := make(map[string]bool, len(routes))
	for _, r := range routes {
		if r.Path == "" {
			return fmt.Errorf("config: route path must not be empty")
		}
		// ServeMux patterns must start with "/", else registration panics.
		if !strings.HasPrefix(r.Path, "/") {
			return fmt.Errorf("config: route path %q must start with /", r.Path)
		}
		if seenPath[r.Path] {
			return fmt.Errorf("config: duplicate route path %q", r.Path)
		}
		seenPath[r.Path] = true

		if !isValidProtocol(r.InputProtocol) {
			return fmt.Errorf("config: route %q has invalid input_protocol %q", r.Path, r.InputProtocol)
		}
		if !isValidRouteOperation(r.Operation) {
			return fmt.Errorf("config: route %q has invalid operation %q", r.Path, r.Operation)
		}
		if r.Operation == RouteOperationResponsesCompact && r.InputProtocol != ProtoResponses {
			return fmt.Errorf("config: route %q operation %q requires input_protocol %q", r.Path, r.Operation, ProtoResponses)
		}
		if len(r.Models) == 0 {
			return fmt.Errorf("config: route %q needs at least one [[route.model]]", r.Path)
		}

		seenExact := make(map[string]bool, len(r.Models))
		catchall := 0
		subscriptionRules := 0
		for _, m := range r.Models {
			if m.Upstream == "" {
				return fmt.Errorf("config: route %q has a model rule with no upstream", r.Path)
			}
			u, ok := byName[m.Upstream]
			if !ok {
				return fmt.Errorf("config: route %q references undefined upstream %q", r.Path, m.Upstream)
			}
			if u.AuthMode == AuthModeCodexSubscription {
				subscriptionRules++
				if r.InputProtocol != ProtoResponses {
					return fmt.Errorf("config: route %q via subscription upstream %q requires input_protocol %q", r.Path, u.Name, ProtoResponses)
				}
				if m.Model == nil || *m.Model != "" {
					return fmt.Errorf("config: route %q via subscription upstream %q requires model = \"\"", r.Path, u.Name)
				}
			}
			// Only responses -> chat conversion is implemented. All other
			// cross-protocol pairs, including Anthropic, are rejected at load.
			if r.InputProtocol != u.Protocol && !(r.InputProtocol == ProtoResponses && u.Protocol == ProtoChat) {
				return fmt.Errorf("config: route %q via upstream %q: protocol %s -> %s not supported",
					r.Path, u.Name, r.InputProtocol, u.Protocol)
			}
			switch {
			case m.Match == "" || m.Match == "*":
				catchall++
			case !isWildcard(m.Match):
				if seenExact[m.Match] {
					return fmt.Errorf("config: route %q has duplicate exact match %q", r.Path, m.Match)
				}
				seenExact[m.Match] = true
			}
		}
		if catchall > 1 {
			return fmt.Errorf("config: route %q has multiple catch-all matches", r.Path)
		}
		if subscriptionRules > 0 {
			if subscriptionRules != len(r.Models) {
				return fmt.Errorf("config: route %q must not mix codex_subscription and normal upstreams", r.Path)
			}
			if len(r.Models) != 1 || r.Models[0].Match != "*" {
				return fmt.Errorf("config: route %q using codex_subscription requires exactly one match = \"*\" rule", r.Path)
			}
		} else if r.Operation == RouteOperationResponsesCompact {
			return fmt.Errorf("config: route %q operation %q requires a codex_subscription upstream", r.Path, r.Operation)
		}
	}
	return nil
}

func validateCodexSubscriptionUpstream(u Upstream) error {
	if u.Protocol != ProtoResponses {
		return fmt.Errorf("config: subscription upstream %q protocol must be %q", u.Name, ProtoResponses)
	}
	if u.BaseURL != CodexSubscriptionBaseURL {
		return fmt.Errorf("config: subscription upstream %q base_url must be %q", u.Name, CodexSubscriptionBaseURL)
	}
	if u.Path != CodexSubscriptionPath {
		return fmt.Errorf("config: subscription upstream %q path must be %q", u.Name, CodexSubscriptionPath)
	}
	if u.APIKey != "" {
		return fmt.Errorf("config: subscription upstream %q api_key must be empty", u.Name)
	}
	if u.Model != "" {
		return fmt.Errorf("config: subscription upstream %q model must be empty", u.Name)
	}
	if u.SupportsResponses || u.ResponsesPath != "" {
		return fmt.Errorf("config: subscription upstream %q must not set supports_responses or responses_path", u.Name)
	}
	return nil
}

func validateRuntimeConfig(cfg Config) error {
	hasSubscription := false
	for _, u := range cfg.Upstreams {
		if u.AuthMode == AuthModeCodexSubscription {
			hasSubscription = true
			break
		}
	}
	if !hasSubscription {
		return nil
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("config: codex_subscription requires APID_LISTEN to be an IP loopback address with a port, got %q", cfg.Listen)
	}
	ip := net.ParseIP(host)
	portNum, portErr := strconv.Atoi(port)
	validLoopback := ip != nil && (ip.Equal(net.IPv6loopback) ||
		(!strings.Contains(host, ":") && ip.To4() != nil && ip.To4()[0] == 127))
	if !validLoopback || portErr != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("config: codex_subscription requires APID_LISTEN to be an IP loopback address with a port, got %q", cfg.Listen)
	}
	return nil
}

func validateSearchConfig(s SearchConfig) error {
	if s.Provider == "" {
		return nil // search is optional
	}
	switch s.Provider {
	case "exa":
		if s.APIKey == "" {
			return fmt.Errorf("config: [search] api_key must not be empty when provider is %q", s.Provider)
		}
	default:
		return fmt.Errorf("config: [search] provider %q not supported (only \"exa\")", s.Provider)
	}
	if s.Path != "" && !strings.HasPrefix(s.Path, "/") {
		return fmt.Errorf("config: [search] path %q must start with /", s.Path)
	}
	return nil
}

func isValidProtocol(p Protocol) bool {
	return p == ProtoResponses || p == ProtoChat || p == ProtoAnthropic
}

func isValidAuthMode(m AuthMode) bool {
	return m == AuthModeDefault || m == AuthModeCodexSubscription
}

func isValidRouteOperation(op RouteOperation) bool {
	return op == RouteOperationInference || op == RouteOperationResponsesCompact
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "on":
		return true
	default:
		return false
	}
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive duration, got %q", key, raw)
	}
	return d, nil
}

func positiveInt64Env(key string, def int64) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive byte count, got %q", key, raw)
	}
	return value, nil
}
