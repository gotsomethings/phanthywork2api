package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"phanthycode2api/internal/auth"
)

func TestParseProxyURL(t *testing.T) {
	good := []string{
		"http://127.0.0.1:8080",
		"http://Default.phanthy_acc1:token@resin:8080",
		"https://proxy.example.com:3128",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	}
	for _, s := range good {
		u, err := ParseProxyURL(s)
		if err != nil || u == nil {
			t.Errorf("ParseProxyURL(%q) = %v, %v; want ok", s, u, err)
		}
	}
	bad := []string{"", "ftp://host:21", "http://", "notaurl"}
	for _, s := range bad {
		u, err := ParseProxyURL(s)
		if s == "" {
			if u != nil || err != nil {
				t.Errorf("empty proxy should be (nil,nil), got %v,%v", u, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseProxyURL(%q) should fail", s)
		}
	}
}

// TestDoUnderWriteLock 回归测试：持 a 写锁的路径（如 CreateAPIKey）调用 do 不得自锁死。
// 早期实现在 do 内再次 a.Lock()，sync.Mutex 不可重入 → 永久阻塞。
func TestDoUnderWriteLock(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	c := New(ts.URL)
	a := &auth.Auth{UID: "u1", AccessToken: "t"}

	done := make(chan error, 1)
	var mu sync.Mutex
	mu.Lock() // 模拟调用方已持写锁
	go func() {
		req, err := http.NewRequest(http.MethodGet, ts.URL, nil)
		if err != nil {
			done <- err
			return
		}
		resp, err := c.do(a, req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("do under lock failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock: do() blocked while caller holds auth write lock")
	}
	mu.Unlock()
}

// TestAccountProxyIsUsed 端到端验证：账号配了代理后，请求确实走代理（而不是直连）。
// 假代理收到的请求 URI 是绝对形式（r.Host 为真实目标主机），据此断言。
func TestAccountProxyIsUsed(t *testing.T) {
	var gotHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host // 代理模式下为绝对 URI 的目标主机
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("via-proxy"))
	}))
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct"))
	}))
	defer target.Close()

	c := New(target.URL)
	a := &auth.Auth{UID: "u", AccessToken: "t"}
	a.SetProxy(proxy.URL)

	req, err := http.NewRequest(http.MethodGet, target.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.do(a, req)
	if err != nil {
		t.Fatalf("do via proxy: %v", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	if string(body[:n]) != "via-proxy" {
		t.Fatalf("request did not go through proxy, body=%q", string(body[:n]))
	}
	if gotHost == "" {
		t.Fatal("proxy did not receive absolute-URI request")
	}
}

// TestEffectiveProxy 账号级代理优先于全局默认。
func TestEffectiveProxy(t *testing.T) {
	c := New("https://code.phanthy.com")
	c.SetOpts(&Opts{DefaultProxy: "http://global:8080"})
	if got := c.effectiveProxy("socks5://acc:1080"); got != "socks5://acc:1080" {
		t.Fatalf("account proxy should win, got %q", got)
	}
	if got := c.effectiveProxy(""); got != "http://global:8080" {
		t.Fatalf("should fall back to global, got %q", got)
	}
}

// TestSetOptsHotUpdate 面板热更新请求头后立即对新请求生效。
func TestSetOptsHotUpdate(t *testing.T) {
	var seen map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = map[string]string{
			"version": r.Header.Get("anthropic-version"),
			"beta":    r.Header.Get("anthropic-beta"),
			"ua":      r.Header.Get("User-Agent"),
			"custom":  r.Header.Get("x-phanthy-client-source"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(ts.URL)
	a := &auth.Auth{UID: "u", AccessToken: "t", APIKey: "k"}
	if _, _, _, err := c.ChatStream(a, []byte(`{"model":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if seen["version"] != "2023-06-01" {
		t.Fatalf("default anthropic-version missing: %v", seen)
	}

	// 热更新
	c.SetOpts(&Opts{
		AnthropicVersion: "2024-01-01",
		BetaHeader:       "claude-code-20250219",
		UserAgent:        "claude-cli/0.3.25",
		ExtraHeaders:     map[string]string{"x-phanthy-client-source": "desktop"},
	})
	seen = nil
	if _, _, _, err := c.ChatStream(a, []byte(`{"model":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if seen["version"] != "2024-01-01" || seen["beta"] != "claude-code-20250219" ||
		seen["ua"] != "claude-cli/0.3.25" || seen["custom"] != "desktop" {
		t.Fatalf("hot update did not take effect: %v", seen)
	}
}
