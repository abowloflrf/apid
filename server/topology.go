package server

import (
	"net/http"
	"strings"

	"github.com/abowloflrf/apid/config"
)

// The topology endpoint exposes the loaded forwarding config as the graph the
// dashboard draws: entrypoint -> model rule -> upstream, with the derived facts
// (forward vs convert, which model id actually goes upstream, how the upstream
// is authenticated) precomputed here so the UI stays presentation-only.
//
// Secrets never cross this boundary: API keys are reported as a mode, not a
// value. The endpoint is read-only and, unlike the stats API, does not need
// storage — the config is always available.

type topologyResponse struct {
	Listen     string          `json:"listen"`
	ClientAuth bool            `json:"client_auth"` // inbound client_api_key is enforced
	Trace      bool            `json:"trace"`
	Storage    bool            `json:"storage"`
	Search     *searchInfo     `json:"search"` // nil when [search] is not configured
	Upstreams  []upstreamInfo  `json:"upstreams"`
	Routes     []routeInfo     `json:"routes"`
	Protocols  []protocolCount `json:"protocols"` // entrypoint protocols in use, for a quick legend
}

type searchInfo struct {
	Provider string `json:"provider"`
	Path     string `json:"path"`
	BaseURL  string `json:"base_url"`
}

type upstreamInfo struct {
	Name              string `json:"name"`
	Protocol          string `json:"protocol"`
	BaseURL           string `json:"base_url"`
	Path              string `json:"path"`
	Endpoint          string `json:"endpoint"`
	SupportsResponses bool   `json:"supports_responses"`           // dual-protocol backend: chat + Responses
	ResponsesEndpoint string `json:"responses_endpoint,omitempty"` // Responses endpoint when supports_responses
	Model             string `json:"model"`                        // upstream-level rewrite; "" = pass through
	Auth              string `json:"auth"`                         // api_key | passthrough | stripped
	AuthHdr           string `json:"auth_header"`                  // header the key is sent in; "" when no key
	AuthMode          string `json:"auth_mode"`
	Experimental      bool   `json:"experimental"`
	RefCount          int    `json:"ref_count"` // model rules pointing here
}

type routeInfo struct {
	Path          string     `json:"path"`
	InputProtocol string     `json:"input_protocol"`
	Operation     string     `json:"operation"`
	Rules         []ruleInfo `json:"rules"`
}

// ruleInfo is one [[route.model]] entry with everything the request path would
// derive from it at dispatch time.
type ruleInfo struct {
	Match          string `json:"match"`      // as written; "" for the implicit catch-all
	MatchKind      string `json:"match_kind"` // exact | glob | catchall
	Upstream       string `json:"upstream"`
	UpstreamProto  string `json:"upstream_protocol"`
	Mode           string `json:"mode"`            // forward | convert
	ViaResponses   bool   `json:"via_responses"`   // raw forward to the upstream's Responses endpoint
	ModelSource    string `json:"model_source"`    // rule | upstream | passthrough
	EffectiveModel string `json:"effective_model"` // "" when the client's model passes through
	Endpoint       string `json:"endpoint"`
	Broken         bool   `json:"broken"` // upstream missing; validation makes this unreachable
}

type protocolCount struct {
	Protocol string `json:"protocol"`
	Routes   int    `json:"routes"`
}

func (s *Server) handleTopology(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.topology())
}

func (s *Server) topology() topologyResponse {
	refs := map[string]int{}
	protoRoutes := map[string]int{}
	var protoOrder []string

	routes := make([]routeInfo, 0, len(s.cfg.Routes))
	for _, rt := range s.cfg.Routes {
		if _, seen := protoRoutes[string(rt.InputProtocol)]; !seen {
			protoOrder = append(protoOrder, string(rt.InputProtocol))
		}
		protoRoutes[string(rt.InputProtocol)]++

		ri := routeInfo{Path: rt.Path, InputProtocol: string(rt.InputProtocol), Operation: string(rt.Operation), Rules: make([]ruleInfo, 0, len(rt.Models))}
		for _, m := range rt.Models {
			refs[m.Upstream]++
			r := ruleInfo{Match: m.Match, MatchKind: matchKind(m.Match), Upstream: m.Upstream}
			tg, ok := s.upstreams[m.Upstream]
			if !ok {
				r.Broken = true
				ri.Rules = append(ri.Rules, r)
				continue
			}
			r.UpstreamProto = string(tg.cfg.Protocol)
			r.Endpoint = tg.client.Endpoint()
			r.Mode = "forward"
			if tg.cfg.AuthMode == config.AuthModeCodexSubscription && rt.Operation == config.RouteOperationResponsesCompact {
				r.Endpoint = tg.client.CompactEndpoint()
			}
			if rt.InputProtocol != tg.cfg.Protocol {
				if rt.InputProtocol == config.ProtoResponses && tg.cfg.SupportsResponses {
					// Dual-protocol backend: no conversion, raw bytes to /v1/responses.
					r.UpstreamProto = string(config.ProtoResponses)
					r.Endpoint = tg.client.ResponsesEndpoint()
					r.ViaResponses = true
				} else {
					r.Mode = "convert"
				}
			}
			r.ModelSource, r.EffectiveModel = modelPlan(m, tg.cfg)
			ri.Rules = append(ri.Rules, r)
		}
		routes = append(routes, ri)
	}

	upstreams := make([]upstreamInfo, 0, len(s.cfg.Upstreams))
	for _, u := range s.cfg.Upstreams {
		ui := upstreamInfo{
			Name:         u.Name,
			Protocol:     string(u.Protocol),
			BaseURL:      u.BaseURL,
			Path:         u.Path,
			Model:        u.Model,
			Auth:         upstreamAuthMode(u, s.apiKey != ""),
			AuthMode:     string(u.AuthMode),
			Experimental: u.AuthMode == config.AuthModeCodexSubscription,
			RefCount:     refs[u.Name],
		}
		if tg, ok := s.upstreams[u.Name]; ok {
			ui.Endpoint = tg.client.Endpoint()
			if u.SupportsResponses {
				ui.SupportsResponses = true
				ui.ResponsesEndpoint = tg.client.ResponsesEndpoint()
			}
		}
		if u.AuthMode == config.AuthModeCodexSubscription {
			ui.AuthHdr = "Authorization: Bearer (client credential)"
		} else if u.APIKey != "" {
			ui.AuthHdr = "Authorization: Bearer"
			if u.Protocol == config.ProtoAnthropic {
				ui.AuthHdr = "X-Api-Key"
			}
		}
		upstreams = append(upstreams, ui)
	}

	protocols := make([]protocolCount, 0, len(protoOrder))
	for _, p := range protoOrder {
		protocols = append(protocols, protocolCount{Protocol: p, Routes: protoRoutes[p]})
	}

	resp := topologyResponse{
		Listen:     s.cfg.Listen,
		ClientAuth: s.apiKey != "",
		Trace:      s.cfg.TraceDir != "",
		Storage:    s.db != nil,
		Upstreams:  upstreams,
		Routes:     routes,
		Protocols:  protocols,
	}
	if s.search != nil {
		resp.Search = &searchInfo{
			Provider: s.cfg.Search.Provider,
			Path:     s.searchPath(),
			BaseURL:  searchBaseURL(s.cfg.Search),
		}
	}
	return resp
}

func matchKind(match string) string {
	switch {
	case match == "" || match == "*":
		return "catchall"
	case strings.Contains(match, "*"):
		return "glob"
	default:
		return "exact"
	}
}

// modelPlan mirrors effectiveModel, but also reports where the decision came
// from so the UI can label it: an explicit "" in a rule forces pass-through,
// which otherwise looks identical to inheriting an empty upstream model.
func modelPlan(m config.ModelRule, u config.Upstream) (source, model string) {
	if m.Model != nil {
		if *m.Model == "" {
			return "passthrough", ""
		}
		return "rule", *m.Model
	}
	if u.Model != "" {
		return "upstream", u.Model
	}
	return "passthrough", ""
}

// upstreamAuthMode describes what reaches the upstream: its own key, the
// client's credentials passed through, or nothing — the last case happens when
// inbound client auth is on, because those headers are scrubbed before
// forwarding (see upstreamClientHeaders).
func upstreamAuthMode(u config.Upstream, clientAuth bool) string {
	switch {
	case u.AuthMode == config.AuthModeCodexSubscription:
		return string(config.AuthModeCodexSubscription)
	case u.APIKey != "":
		return "api_key"
	case clientAuth:
		return "stripped"
	default:
		return "passthrough"
	}
}

func searchBaseURL(sc config.SearchConfig) string {
	if sc.BaseURL != "" {
		return sc.BaseURL
	}
	return "https://api.exa.ai"
}
