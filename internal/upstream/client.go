// Package upstream 负责把请求转发给远程上游服务。
// 一个 Client 绑定一条路由的上游目标（baseURL + path + apiKey），
// 既可转发转换后的 Chat 请求，也可转发任意原始字节（纯转发）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/abowloflrf/apid/internal/types"
)

type Client struct {
	baseURL string
	path    string
	apiKey  string
	http    *http.Client
}

// New 构造一个绑定到 baseURL+path 的上游客户端。
func New(baseURL, path, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		path:    ensureLeadingSlash(path),
		apiKey:  apiKey,
		http:    &http.Client{},
	}
}

// Endpoint 返回实际转发的完整 URL（baseURL + path）。
// 不发起请求，仅供 stats 等调用方记录「实际转发 URL」。
func (c *Client) Endpoint() string {
	return c.baseURL + c.path
}

// Forward 把原始 body 字节 POST 到 Endpoint()。
// 鉴权优先用配置的 apiKey；apiKey 为空时才回退到透传客户端凭证 authOverride。
// 调用方负责关闭返回的 resp.Body。
func (c *Client) Forward(ctx context.Context, body []byte, authOverride string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	switch {
	case c.apiKey != "":
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	case authOverride != "":
		httpReq.Header.Set("Authorization", authOverride)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	return resp, nil
}

// ChatCompletions 把转换后的 Chat 请求序列化后转发给上游。
// 是 Forward 的薄封装，供协议转换路由使用。
func (c *Client) ChatCompletions(ctx context.Context, req *types.ChatRequest, authOverride string) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return c.Forward(ctx, body, authOverride)
}

func ensureLeadingSlash(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}
