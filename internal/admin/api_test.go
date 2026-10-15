package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"phanthycode2api/internal/auth"
	"phanthycode2api/internal/claims"
	"phanthycode2api/internal/conf"
	"phanthycode2api/internal/pool"
	"phanthycode2api/internal/upstream"
)

func newTestAdmin(t *testing.T, apiKey string) (*Admin, *pool.Pool, *conf.Manager) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "acc1", AccessToken: "t1", APIKey: "k1",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "acc2", AccessToken: "t2", APIKey: "k2",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	c := conf.Default()
	m := conf.NewManager(c, "")
	up := upstream.New("https://code.phanthy.com")
	return New(Config{
		Manager:  m,
		Pool:     p,
		Upstream: up,
		Claims:   claims.Load(""),
		ClaimNow: func() {},
		APIKey:   apiKey,
		Version:  "test",
	}), p, m
}

func do(adm *Admin, method, path, body, key string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	adm.ServeHTTP(w, r)
	return w
}

// TestPageServed 面板页面可访问（不含密钥，无需鉴权）。
func TestPageServed(t *testing.T) {
	adm, _, _ := newTestAdmin(t, "secret")
	w := httptest.NewRecorder()
	adm.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("page status=%d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(w.Body.String(), "phanthycode2api") {
		t.Fatal("page body missing title")
	}
}

// TestAPIRequiresKey 配置了 api_key 时管理 API 必须鉴权。
func TestAPIRequiresKey(t *testing.T) {
	adm, _, _ := newTestAdmin(t, "secret")
	if w := do(adm, http.MethodGet, "/admin/api/overview", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key should be 401, got %d", w.Code)
	}
	if w := do(adm, http.MethodGet, "/admin/api/overview", "", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key should be 401, got %d", w.Code)
	}
	if w := do(adm, http.MethodGet, "/admin/api/overview", "", "secret"); w.Code != http.StatusOK {
		t.Fatalf("correct key should be 200, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestOverview 概览返回账号统计。
func TestOverview(t *testing.T) {
	adm, _, _ := newTestAdmin(t, "")
	w := do(adm, http.MethodGet, "/admin/api/overview", "", "")
	var d map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("bad json: %v (%s)", err, w.Body.String())
	}
	acc := d["accounts"].(map[string]any)
	if acc["total"].(float64) != 2 {
		t.Fatalf("total=%v", acc["total"])
	}
	if _, ok := d["safety"]; !ok {
		t.Fatal("missing safety section")
	}
	if _, ok := d["claim"]; !ok {
		t.Fatal("missing claim section")
	}
}

// TestAccountEnableDisable 解禁 / 禁用 / 清冷却。
func TestAccountEnableDisable(t *testing.T) {
	adm, p, _ := newTestAdmin(t, "")
	if w := do(adm, http.MethodPost, "/admin/api/accounts/acc1/disable", `{"reason":"manual"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("disable failed: %d %s", w.Code, w.Body.String())
	}
	if st, _ := p.Status("acc1"); !st.Disabled {
		t.Fatal("acc1 should be disabled")
	}
	if w := do(adm, http.MethodPost, "/admin/api/accounts/acc1/enable", "{}", ""); w.Code != http.StatusOK {
		t.Fatalf("enable failed: %d %s", w.Code, w.Body.String())
	}
	if st, _ := p.Status("acc1"); st.Disabled {
		t.Fatal("acc1 should be re-enabled")
	}
	if w := do(adm, http.MethodPost, "/admin/api/accounts/nope/enable", "{}", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown uid should be 404, got %d", w.Code)
	}
}

// TestSetAccountProxyHotUpdate 给账号设代理后配置即时更新并落盘意图正确。
func TestSetAccountProxyHotUpdate(t *testing.T) {
	adm, p, m := newTestAdmin(t, "")
	w := do(adm, http.MethodPut, "/admin/api/accounts/acc1/proxy",
		`{"proxy":"http://Default.acc1:tok@127.0.0.1:8080","persist":false}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("set proxy failed: %d %s", w.Code, w.Body.String())
	}
	if got := m.Get().Proxy.ByAccount["acc1"]; got != "http://Default.acc1:tok@127.0.0.1:8080" {
		t.Fatalf("config not updated: %q", got)
	}
	if pxy, ok := p.AccountProxy("acc1"); !ok || pxy == "" {
		t.Fatalf("account proxy not applied to pool: %q %v", pxy, ok)
	}
}

// TestConfigPatchAndPersist 局部更新配置（patch）后立即生效。
func TestConfigPatchAndPersist(t *testing.T) {
	adm, _, m := newTestAdmin(t, "")
	w := do(adm, http.MethodPut, "/admin/api/config",
		`{"persist":false,"patch":{"safety":{"concurrency":4,"daily_limit":50}}}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("patch failed: %d %s", w.Code, w.Body.String())
	}
	c := m.Get()
	if c.Safety.Concurrency != 4 || c.Safety.DailyLimit != 50 {
		t.Fatalf("config not applied: %+v", c.Safety)
	}
	// 非法值应被拒绝且不生效
	bad := do(adm, http.MethodPut, "/admin/api/config",
		`{"persist":false,"patch":{"cooldown":{"soft_rate":"abc"}}}`, "")
	if bad.Code == http.StatusOK {
		t.Fatal("invalid duration should be rejected")
	}
	if m.Get().Safety.Concurrency != 4 {
		t.Fatal("failed patch must not change config")
	}
}

// TestClaimsEndpoints 签到状态与手动触发。
func TestClaimsEndpoints(t *testing.T) {
	adm, _, _ := newTestAdmin(t, "")
	if w := do(adm, http.MethodGet, "/admin/api/claims", "", ""); w.Code != http.StatusOK {
		t.Fatalf("claims status: %d", w.Code)
	}
	if w := do(adm, http.MethodPost, "/admin/api/claims/run", "{}", ""); w.Code != http.StatusOK {
		t.Fatalf("claims run: %d %s", w.Code, w.Body.String())
	}
}
