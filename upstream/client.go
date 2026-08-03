// Package upstream 负责把请求转发给远程上游服务。
// 一个 Client 绑定一条路由的上游目标（baseURL + path + apiKey），
// 既可转发转换后的 Chat 请求，也可转发任意原始字节（纯转发）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/abowloflrf/apid/protocol"
)

type Client struct {
	baseURL       string
	path          string
	responsesPath string // optional second endpoint (OpenAI Responses); "" = not supported
	apiKey        string
	authStyle     authStyle
	http          *http.Client
	subscription  *url.URL
}

type authStyle int

const (
	authBearer authStyle = iota
	authXAPIKey
	authCodexSubscription
)

const codexSubscriptionBaseURL = "https://chatgpt.com/backend-api/codex"

// Option adjusts Client behavior for protocol-specific forwarding details.
type Option func(*Client)

// CodexSubscriptionOption adjusts the fixed-target subscription client without
// making the destination configurable.
type CodexSubscriptionOption func(*Client)

// WithXAPIKeyAuth makes configured api_key use Anthropic's X-Api-Key header.
func WithXAPIKeyAuth() Option {
	return func(c *Client) {
		c.authStyle = authXAPIKey
	}
}

// WithResponsesPath gives the client a second, OpenAI Responses endpoint
// (baseURL + path) alongside the primary one. Callers use
// ForwardResponsesWithQuery when one backend speaks both protocols.
func WithResponsesPath(path string) Option {
	return func(c *Client) {
		c.responsesPath = ensureLeadingSlash(path)
	}
}

// WithCodexSubscriptionRoundTripper replaces the transport while keeping the
// production URL fixed. It is primarily useful for deterministic tests.
func WithCodexSubscriptionRoundTripper(rt http.RoundTripper) CodexSubscriptionOption {
	return func(c *Client) {
		c.http.Transport = rt
	}
}

// New 构造一个绑定到 baseURL+path 的上游客户端。
func New(baseURL, path, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		path:      ensureLeadingSlash(path),
		apiKey:    apiKey,
		authStyle: authBearer,
		http:      &http.Client{Transport: newTransport()},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// NewCodexSubscription builds the credential-passthrough client for the fixed
// ChatGPT Codex backend. It never reads proxy environment variables and never
// follows redirects.
func NewCodexSubscription(opts ...CodexSubscriptionOption) *Client {
	base, err := url.Parse(codexSubscriptionBaseURL)
	if err != nil {
		panic("upstream: invalid built-in Codex subscription URL: " + err.Error())
	}
	c := &Client{
		baseURL:      codexSubscriptionBaseURL,
		path:         "/responses",
		authStyle:    authCodexSubscription,
		subscription: base,
		http: &http.Client{
			Transport: newSubscriptionTransport(),
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// newTransport builds a Transport with fine-grained timeouts that bound only
// connection setup and the wait for response headers (i.e. TTFT). It must NOT
// cap the overall request, so long-running streaming (SSE) responses can stay
// open for many minutes. That is also why we never set http.Client.Timeout.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // allow slow-TTFT reasoning models
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		ForceAttemptHTTP2:     true,
	}
}

func newSubscriptionTransport() *http.Transport {
	t := newTransport()
	t.Proxy = nil
	return t
}

// Endpoint 返回实际转发的完整 URL（baseURL + path）。
// 不发起请求，仅供 stats 等调用方记录「实际转发 URL」。
func (c *Client) Endpoint() string {
	return c.baseURL + c.path
}

// EndpointWithQuery returns Endpoint plus the original client query string.
func (c *Client) EndpointWithQuery(rawQuery string) string {
	if rawQuery == "" {
		return c.Endpoint()
	}
	return c.Endpoint() + "?" + rawQuery
}

// SupportsResponses reports whether a dedicated Responses endpoint is configured.
func (c *Client) SupportsResponses() bool {
	return c.responsesPath != ""
}

// ResponsesEndpoint returns the actual Responses endpoint URL (baseURL + responses path).
func (c *Client) ResponsesEndpoint() string {
	return c.baseURL + c.responsesPath
}

// ResponsesEndpointWithQuery returns ResponsesEndpoint plus the original query.
func (c *Client) ResponsesEndpointWithQuery(rawQuery string) string {
	if rawQuery == "" {
		return c.ResponsesEndpoint()
	}
	return c.ResponsesEndpoint() + "?" + rawQuery
}

// CompactEndpoint returns the fixed subscription compact endpoint. It is empty
// for normal upstream clients.
func (c *Client) CompactEndpoint() string {
	if c.subscription == nil {
		return ""
	}
	return c.subscriptionURL(true, "").String()
}

// Forward POSTs the raw body bytes to Endpoint().
// Client headers are passed through except auth/transport/CDN ones (see
// skipForwardHeader). For auth the configured apiKey wins; if empty, the
// client's Authorization is forwarded. Caller closes resp.Body.
func (c *Client) Forward(ctx context.Context, body []byte, clientHeader http.Header) (*http.Response, error) {
	return c.ForwardWithQuery(ctx, body, clientHeader, "")
}

// ForwardWithQuery is Forward plus the client's raw URL query. Pure forwarding
// must preserve query parameters because some upstream-compatible APIs use them
// as feature switches.
func (c *Client) ForwardWithQuery(ctx context.Context, body []byte, clientHeader http.Header, rawQuery string) (*http.Response, error) {
	return c.post(ctx, c.EndpointWithQuery(rawQuery), body, clientHeader)
}

// ForwardResponsesWithQuery is ForwardWithQuery but posts to the dedicated
// Responses endpoint (see WithResponsesPath). Returns an error when no
// Responses endpoint was configured.
func (c *Client) ForwardResponsesWithQuery(ctx context.Context, body []byte, clientHeader http.Header, rawQuery string) (*http.Response, error) {
	if c.responsesPath == "" {
		return nil, fmt.Errorf("upstream: responses endpoint not configured")
	}
	return c.post(ctx, c.ResponsesEndpointWithQuery(rawQuery), body, clientHeader)
}

// ForwardCodexSubscription posts an inference or compact request to one of the
// two fixed subscription endpoints.
func (c *Client) ForwardCodexSubscription(ctx context.Context, body []byte, clientHeader http.Header, rawQuery string, compact bool) (*http.Response, error) {
	if c.authStyle != authCodexSubscription || c.subscription == nil {
		return nil, fmt.Errorf("upstream: Codex subscription client not configured")
	}
	auth, err := singleBearerAuthorization(clientHeader)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.subscriptionURL(compact, rawQuery).String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("upstream: invalid Codex subscription query")
	}
	// Prevent net/http from making the POST replayable even if a future caller
	// adds an idempotency header.
	httpReq.GetBody = nil
	copySubscriptionHeaders(httpReq.Header, clientHeader)
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Authorization", auth)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream: Codex subscription request failed")
	}
	return resp, nil
}

func (c *Client) subscriptionURL(compact bool, rawQuery string) *url.URL {
	u := *c.subscription
	u.Path = strings.TrimRight(u.Path, "/") + "/responses"
	if compact {
		u.Path += "/compact"
	}
	u.RawPath = ""
	u.RawQuery = rawQuery
	u.Fragment = ""
	return &u
}

func (c *Client) post(ctx context.Context, url string, body []byte, clientHeader http.Header) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyForwardHeaders(httpReq.Header, clientHeader)
	httpReq.Header.Set("Content-Type", "application/json")

	c.applyAuth(httpReq.Header, clientHeader)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	return resp, nil
}

func (c *Client) applyAuth(dst, clientHeader http.Header) {
	switch c.authStyle {
	case authXAPIKey:
		switch {
		case c.apiKey != "":
			dst.Set("X-Api-Key", c.apiKey)
		case clientHeader.Get("X-Api-Key") != "":
			dst.Set("X-Api-Key", clientHeader.Get("X-Api-Key"))
		case clientHeader.Get("Api-Key") != "":
			dst.Set("X-Api-Key", clientHeader.Get("Api-Key"))
		case clientHeader.Get("Authorization") != "":
			dst.Set("Authorization", clientHeader.Get("Authorization"))
		}
	default:
		switch {
		case c.apiKey != "":
			dst.Set("Authorization", "Bearer "+c.apiKey)
		case clientHeader.Get("Authorization") != "":
			dst.Set("Authorization", clientHeader.Get("Authorization"))
		}
	}
}

// ChatCompletions 把转换后的 Chat 请求序列化后转发给上游。
// 是 Forward 的薄封装，供协议转换路由使用。
func (c *Client) ChatCompletions(ctx context.Context, req *protocol.ChatRequest, clientHeader http.Header) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return c.Forward(ctx, body, clientHeader)
}

// copyForwardHeaders copies client headers to the upstream request, skipping the
// ones owned by us or the transport (see skipForwardHeader).
func copyForwardHeaders(dst, src http.Header) {
	for k, vs := range src {
		if skipForwardHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func copySubscriptionHeaders(dst, src http.Header) {
	for k, vs := range src {
		if skipSubscriptionHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func singleBearerAuthorization(h http.Header) (string, error) {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return "", fmt.Errorf("upstream: Codex subscription requires exactly one Bearer Authorization header")
	}
	v := strings.TrimSpace(values[0])
	if strings.Contains(v, ",") {
		return "", fmt.Errorf("upstream: Codex subscription requires exactly one Bearer Authorization header")
	}
	fields := strings.Fields(v)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || fields[1] == "" {
		return "", fmt.Errorf("upstream: Codex subscription requires exactly one Bearer Authorization header")
	}
	return v, nil
}

// skipForwardHeader reports whether a client header must not be forwarded:
// auth (apid manages auth), transport-owned headers (Host, Content-*, hop-by-hop),
// Accept-Encoding (let the Go transport negotiate gzip and decompress for us),
// and CDN/tracing headers that would leak the client's IP or carry stale routing.
func skipForwardHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Authorization", "Proxy-Authorization", "Api-Key", "X-Api-Key",
		"Host", "Content-Length", "Content-Type", "Transfer-Encoding",
		"Connection", "Keep-Alive", "Te", "Trailer", "Upgrade", "Proxy-Authenticate",
		"Accept-Encoding",
		"Forwarded", "True-Client-Ip":
		return true
	}
	// CDN / proxy hop prefixes, e.g. X-Forwarded-For, CF-Connecting-IP.
	for _, p := range cdnHeaderPrefixes {
		if len(key) >= len(p) && strings.EqualFold(key[:len(p)], p) {
			return true
		}
	}
	return false
}

func skipSubscriptionHeader(key string) bool {
	canonical := http.CanonicalHeaderKey(key)
	switch canonical {
	case "Authorization", "Proxy-Authorization", "Api-Key", "X-Api-Key", "X-Apid-Key",
		"Cookie", "Set-Cookie", "Host", "Content-Length", "Transfer-Encoding",
		"Connection", "Keep-Alive", "Te", "Trailer", "Upgrade", "Proxy-Authenticate",
		"Accept-Encoding", "Forwarded", "True-Client-Ip", "X-Request-Id",
		"Traceparent", "Tracestate", "Baggage", "X-Amzn-Trace-Id",
		"Uber-Trace-Id", "X-Cloud-Trace-Context":
		return true
	}
	for _, p := range append(cdnHeaderPrefixes, "X-B3-") {
		if len(key) >= len(p) && strings.EqualFold(key[:len(p)], p) {
			return true
		}
	}
	return false
}

var cdnHeaderPrefixes = []string{"X-Forwarded-", "CF-"}

func ensureLeadingSlash(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}
