// proxy.go 按账号代理支持：每个代理 URL 缓存一个 *http.Transport，
// 让同一账号的所有出站请求（chat/oauth/keepalive/签到）固定走同一出口 IP。
package upstream

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// proxyCache 按代理 URL 缓存 transport，避免每请求重建连接池。
var proxyCache = struct {
	sync.Mutex
	m map[string]*http.Transport
}{m: map[string]*http.Transport{}}

// ParseProxyURL 校验代理 URL 格式，非法返回错误。
// 支持 http / https / socks5 / socks5h（ socks5h = 远程 DNS 解析，推荐）。
func ParseProxyURL(proxyURL string) (*url.URL, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5/socks5h)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q missing host", proxyURL)
	}
	return u, nil
}

// DoWithProxy 用指定代理发一次请求（供管理面板测试代理连通性）。
// 调用方负责关闭 resp.Body。
func DoWithProxy(proxyURL string, req *http.Request) (*http.Response, error) {
	if strings.TrimSpace(proxyURL) == "" {
		return http.DefaultClient.Do(req)
	}
	tr, err := transportFor(proxyURL)
	if err != nil {
		return nil, err
	}
	return (&http.Client{Timeout: 20 * time.Second, Transport: tr}).Do(req)
}

// ForgetProxy 丢弃某个代理的缓存连接池（改代理/下线代理后调用，释放连接）。
func ForgetProxy(proxyURL string) {
	proxyCache.Lock()
	defer proxyCache.Unlock()
	delete(proxyCache.m, strings.TrimSpace(proxyURL))
}

// transportFor 返回绑定指定代理的 transport；proxyURL 为空返回 (nil, nil)。
func transportFor(proxyURL string) (*http.Transport, error) {
	u, err := ParseProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, nil
	}
	proxyCache.Lock()
	defer proxyCache.Unlock()
	if tr, ok := proxyCache.m[proxyURL]; ok {
		return tr, nil
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(u),
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	proxyCache.m[proxyURL] = tr
	return tr, nil
}
