// Package config loads runtime configuration: ops params (listen addr, SQLite,
// TRACE) from env vars; forwarding config from a TOML file with two tables:
//   - [[upstream]]: a backend deployment (protocol/addr/auth/model), defined once
//     and referenced by name.
//   - [[route]]: a public entrypoint (path + client protocol) that dispatches by
//     the request's model to one upstream.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/joho/godotenv"
)

// Protocol is the API protocol spoken on one end.
type Protocol string

const (
	ProtoResponses Protocol = "openai_responses"
	ProtoChat      Protocol = "openai_chat_completions"
)

type Config struct {
	Listen    string // APID_LISTEN
	TraceDir  string // APID_TRACE_DIR / APID_TRACE; empty = off
	DB        string // APID_DB; empty = off
	Upstreams []Upstream
	Routes    []Route
}

// Upstream is a backend deployment, referenced by Name from any number of routes
// so its address/auth/model are written once, not per entrypoint.
type Upstream struct {
	Name     string   `toml:"name"`
	Protocol Protocol `toml:"protocol"`
	BaseURL  string   `toml:"base_url"`
	Path     string   `toml:"path"`
	APIKey   string   `toml:"api_key"` // empty = pass through client Authorization
	Model    string   `toml:"model"`   // empty = pass through client model
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
	Path          string      `toml:"path"`
	InputProtocol Protocol    `toml:"input_protocol"`
	Models        []ModelRule `toml:"model"`
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
	Upstreams []Upstream `toml:"upstream"`
	Routes    []Route    `toml:"route"`
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

	upstreams, routes, err := loadFile(configPath)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Listen:    env("APID_LISTEN", ":8080"),
		TraceDir:  traceDir,
		DB:        env("APID_DB", ""),
		Upstreams: upstreams,
		Routes:    routes,
	}, nil
}

func loadFile(path string) ([]Upstream, []Route, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("config: read %q failed (set --config): %w", path, err)
	}
	var fc fileConfig
	md, err := toml.Decode(string(data), &fc)
	if err != nil {
		return nil, nil, fmt.Errorf("config: parse %q failed: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return nil, nil, fmt.Errorf("config: %q has unknown keys: %v", path, undec)
	}
	if err := validateConfig(fc.Upstreams, fc.Routes); err != nil {
		return nil, nil, err
	}
	return fc.Upstreams, fc.Routes, nil
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
		if u.BaseURL == "" {
			return fmt.Errorf("config: upstream %q base_url must not be empty", u.Name)
		}
		if u.Path == "" {
			return fmt.Errorf("config: upstream %q path must not be empty", u.Name)
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
		if len(r.Models) == 0 {
			return fmt.Errorf("config: route %q needs at least one [[route.model]]", r.Path)
		}

		seenExact := make(map[string]bool, len(r.Models))
		catchall := 0
		for _, m := range r.Models {
			if m.Upstream == "" {
				return fmt.Errorf("config: route %q has a model rule with no upstream", r.Path)
			}
			u, ok := byName[m.Upstream]
			if !ok {
				return fmt.Errorf("config: route %q references undefined upstream %q", r.Path, m.Upstream)
			}
			// Reverse conversion (chat -> responses) is not implemented.
			if r.InputProtocol == ProtoChat && u.Protocol == ProtoResponses {
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
	}
	return nil
}

func isValidProtocol(p Protocol) bool {
	return p == ProtoResponses || p == ProtoChat
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
