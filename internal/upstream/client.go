// Package upstream 负责把转换后的请求转发给远程 Chat Completions 服务。
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
	apiKey  string
	http    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{},
	}
}

// ChatCompletions 向上游发起一次 chat/completions 请求。
// 鉴权优先用配置的 apiKey；apiKey 为空时才回退到透传客户端凭证 authOverride。
// 调用方负责关闭返回的 resp.Body。
func (c *Client) ChatCompletions(ctx context.Context, req *types.ChatRequest, authOverride string) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	url := c.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
