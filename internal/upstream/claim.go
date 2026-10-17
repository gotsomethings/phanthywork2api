// claim.go 每日签到 / 积分查询。
// 端点来自官方桌面端（claude-sidecar）的 API 路由表：
//
//	POST /api/oauth/activities/daily-login/claim   每日签到领取
//	GET  /api/oauth/rewards                        奖励/积分
//	GET  /api/oauth/activities/summary             活动概览
//
// 认证与现有 OAuth 接口一致：Bearer access_token + anthropic-version。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"phanthycode2api/internal/auth"
)

// DailyClaim 执行每日签到。idempotent 未证实，重复领取按上游返回处理。
func (c *Client) DailyClaim(a *auth.Auth) (map[string]any, error) {
	a.Lock()
	token := strings.TrimSpace(a.AccessToken)
	proxy := a.Proxy
	a.Unlock()
	if token == "" {
		return nil, fmt.Errorf("no accessToken")
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseAPIURL+"/api/oauth/activities/daily-login/claim", nil)
	if err != nil {
		return nil, err
	}
	o := c.Opts()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", c.OAuthBetaHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", o.UserAgent)
	for k, v := range o.ExtraHeaders {
		req.Header.Set(k, v)
	}
	proxyURL := c.effectiveProxy(proxy)
	resp, err := c.doWithProxyControl(proxyURL, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &Error{Kind: ErrClient, Status: resp.StatusCode, Msg: "daily-login claim: non-json response: " + truncate(string(raw), 120)}
	}
	return m, nil
}

// Rewards 查询账号奖励/积分。
func (c *Client) Rewards(a *auth.Auth) (map[string]any, error) {
	a.Lock()
	token := strings.TrimSpace(a.AccessToken)
	proxy := a.Proxy
	a.Unlock()
	if token == "" {
		return nil, fmt.Errorf("no accessToken")
	}
	req, err := http.NewRequest(http.MethodGet, c.BaseAPIURL+"/api/oauth/rewards", nil)
	if err != nil {
		return nil, err
	}
	o := c.Opts()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", c.OAuthBetaHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", o.UserAgent)
	for k, v := range o.ExtraHeaders {
		req.Header.Set(k, v)
	}
	proxyURL := c.effectiveProxy(proxy)
	resp, err := c.doWithProxyControl(proxyURL, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, &Error{Kind: ErrClient, Status: resp.StatusCode, Msg: "rewards: non-json response"}
	}
	return m, nil
}
