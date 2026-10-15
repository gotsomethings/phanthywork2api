// Package admin 控制面板：管理 API + 单文件页面托管。
//
// 安全说明：页面本体（/admin/）是纯静态、不含任何密钥，直接可访问；
// 所有 /admin/api/* 在配置了 api_key 时必须带 Authorization: Bearer <key>，
// 与 OpenAI 兼容接口共用同一把钥匙。
package admin

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"phanthycode2api/internal/claims"
	"phanthycode2api/internal/conf"
	"phanthycode2api/internal/pool"
	"phanthycode2api/internal/upstream"
)

// Config admin 依赖。
type Config struct {
	Manager  *conf.Manager
	Pool     *pool.Pool
	Upstream *upstream.Client
	Claims   *claims.Store // 签到记录（nil = 未启用）
	ClaimNow func()        // 手动触发签到
	APIKey   string        // 空 = 不鉴权
	Version  string
}

// Admin 面板服务。
type Admin struct {
	cfg Config
	mux *http.ServeMux
}

// New 构建。
func New(cfg Config) *Admin {
	a := &Admin{cfg: cfg, mux: http.NewServeMux()}
	a.mux.HandleFunc("/admin/", a.page)
	a.mux.HandleFunc("/admin/api/overview", a.withAuth(a.overview))
	a.mux.HandleFunc("/admin/api/config", a.withAuth(a.configRW))
	a.mux.HandleFunc("/admin/api/reload", a.withAuth(a.reload))
	a.mux.HandleFunc("/admin/api/accounts", a.withAuth(a.accounts))
	a.mux.HandleFunc("/admin/api/accounts/", a.withAuth(a.accountAction))
	a.mux.HandleFunc("/admin/api/proxy/test", a.withAuth(a.proxyTest))
	a.mux.HandleFunc("/admin/api/claims", a.withAuth(a.claimsStatus))
	a.mux.HandleFunc("/admin/api/claims/run", a.withAuth(a.claimsRun))
	a.mux.HandleFunc("/admin/api/rewards/", a.withAuth(a.accountRewards))
	return a
}

func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

func (a *Admin) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != a.cfg.APIKey {
				writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 概览
// ---------------------------------------------------------------------------

func (a *Admin) overview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	c := a.cfg.Manager.Get()
	rt := a.cfg.Manager.Runtime()
	st := a.cfg.Pool.List()

	healthy, cooling, disabled := 0, 0, 0
	todayUsed := 0
	accounts := make([]map[string]any, 0, len(st))
	for _, s := range st {
		switch {
		case s.Disabled:
			disabled++
		case s.Cooling:
			cooling++
		default:
			healthy++
		}
		todayUsed += s.Daily
		proxy, _ := a.cfg.Pool.AccountProxy(s.UID)
		accounts = append(accounts, map[string]any{
			"uid":       s.UID,
			"nickname":  s.Nickname,
			"has_api":   s.HasAPI,
			"state":     accountState(s),
			"until":     s.Until.Format(time.RFC3339),
			"reason":    s.Reason,
			"err_count": s.ErrCount,
			"daily":     s.Daily,
			"proxy":     maskProxy(proxy),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":  a.cfg.Version,
		"time":     time.Now().Format(time.RFC3339),
		"upstream": c.BaseURL,
		"listen":   c.Listen,
		"accounts": map[string]any{
			"total":      len(st),
			"healthy":    healthy,
			"cooling":    cooling,
			"disabled":   disabled,
			"today_used": todayUsed,
			"list":       accounts,
		},
		"safety": map[string]any{
			"daily_limit":           c.Safety.DailyLimit,
			"concurrency":           c.Safety.Concurrency,
			"jitter_max":            c.Safety.JitterMax,
			"slot_wait":             c.Safety.SlotWait,
			"max_rotate":            rt.MaxRotate,
			"block_on_risk_control": rt.BlockOnRisk,
			"timeout":               rt.Timeout.String(),
		},
		"proxy": map[string]any{
			"default":    maskProxy(c.Proxy.Default),
			"by_account": len(c.Proxy.ByAccount),
		},
		"claim":       claimOverview(a.cfg),
		"config_path": a.cfg.Manager.Path(),
	})
}

func accountState(s pool.Status) string {
	switch {
	case s.Disabled:
		return "disabled"
	case s.Cooling:
		return "cooling"
	default:
		return "healthy"
	}
}

// maskProxy 隐藏代理认证信息，只留 scheme://host:port。
func maskProxy(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if i := strings.Index(p, "@"); i > 0 {
		scheme := p[:strings.Index(p, "://")+3]
		return scheme + "***@" + p[i+1:]
	}
	return p
}

func claimOverview(cfg Config) map[string]any {
	hours := []int{}
	if cfg.Manager != nil {
		hours = cfg.Manager.Get().Schedule.ClaimHours
	}
	out := map[string]any{"enabled": len(hours) > 0, "hours": hours}
	if cfg.Claims != nil {
		snap := cfg.Claims.Snapshot(7)
		today := time.Now().Format("2006-01-02")
		ok, fail := 0, 0
		if m, exists := snap[today]; exists {
			for _, r := range m {
				if r.OK {
					ok++
				} else {
					fail++
				}
			}
		}
		out["today_ok"] = ok
		out["today_fail"] = fail
		out["recent"] = snap
	}
	return out
}

// ---------------------------------------------------------------------------
// 配置读写 / 热重载
// ---------------------------------------------------------------------------

func (a *Admin) configRW(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c := a.cfg.Manager.Get()
		writeJSON(w, http.StatusOK, map[string]any{"config": c.JSONDoc(), "path": a.cfg.Manager.Path()})
	case http.MethodPut, http.MethodPatch, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		var payload struct {
			Config  map[string]any `json:"config"`
			Patch   map[string]any `json:"patch"`
			Persist *bool          `json:"persist"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		patch := payload.Patch
		if patch == nil {
			patch = payload.Config
		}
		if patch == nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "missing `patch` or `config`")
			return
		}
		persist := true
		if payload.Persist != nil {
			persist = *payload.Persist
		}
		if err := a.cfg.Manager.Update(func(c *conf.Config) error {
			return c.ApplyPatch(patch)
		}, persist); err != nil {
			writeErr(w, http.StatusBadRequest, "config_update_failed", err.Error())
			return
		}
		log.Printf("admin: config updated (persist=%v) from %s", persist, r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": a.cfg.Manager.Get().JSONDoc()})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/PUT only")
	}
}

func (a *Admin) reload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if err := a.cfg.Manager.Reload(); err != nil {
		writeErr(w, http.StatusBadRequest, "reload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": a.cfg.Manager.Get().JSONDoc()})
}

// ---------------------------------------------------------------------------
// 账号管理
// ---------------------------------------------------------------------------

func (a *Admin) accounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	st := a.cfg.Pool.List()
	out := make([]map[string]any, 0, len(st))
	for _, s := range st {
		proxy, _ := a.cfg.Pool.AccountProxy(s.UID)
		out = append(out, map[string]any{
			"uid": s.UID, "nickname": s.Nickname, "state": accountState(s),
			"has_api": s.HasAPI, "reason": s.Reason, "err_count": s.ErrCount,
			"daily": s.Daily, "until": s.Until.Format(time.RFC3339),
			"proxy": maskProxy(proxy),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// accountAction /admin/api/accounts/{uid}/{action}
// action: enable | disable | clear-cooldown | proxy(PUT)
func (a *Admin) accountAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/api/accounts/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing uid")
		return
	}
	uid := parts[0]
	action := "proxy"
	if len(parts) > 1 {
		action = parts[1]
	}

	// 单账号签到 / 积分：/admin/api/accounts/{uid}/claim、/admin/api/rewards/{uid}
	if action == "claim" {
		a.accountClaim(w, r)
		return
	}

	switch action {
	case "enable":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
			return
		}
		if !a.cfg.Pool.Enable(uid) {
			writeErr(w, http.StatusNotFound, "not_found", "uid not in pool")
			return
		}
		// 同步解除配置里的账号级禁用记录（若有）
		_ = a.cfg.Manager.Update(func(c *conf.Config) error { return nil }, false)
		log.Printf("admin: account %s re-enabled", uid)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "state": "healthy"})

	case "disable":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
			return
		}
		var b struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&b)
		if b.Reason == "" {
			b.Reason = "disabled from admin panel"
		}
		a.cfg.Pool.Disable(uid, b.Reason)
		log.Printf("admin: account %s disabled (%s)", uid, b.Reason)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "state": "disabled"})

	case "clear-cooldown":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
			return
		}
		if !a.cfg.Pool.ClearCooldown(uid) {
			writeErr(w, http.StatusNotFound, "not_found", "uid not in pool")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})

	case "proxy":
		if r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != http.MethodDelete {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "PUT only")
			return
		}
		var b struct {
			Proxy   string `json:"proxy"`
			Persist *bool  `json:"persist"`
		}
		if r.Method != http.MethodDelete {
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&b)
		}
		persist := true
		if b.Persist != nil {
			persist = *b.Persist
		}
		if err := a.cfg.Manager.Update(func(c *conf.Config) error {
			if c.Proxy.ByAccount == nil {
				c.Proxy.ByAccount = map[string]string{}
			}
			if strings.TrimSpace(b.Proxy) == "" {
				delete(c.Proxy.ByAccount, uid)
			} else {
				c.Proxy.ByAccount[uid] = strings.TrimSpace(b.Proxy)
			}
			return nil
		}, persist); err != nil {
			writeErr(w, http.StatusBadRequest, "update_failed", err.Error())
			return
		}
		// 立即应用到账号（不依赖 main 的回调，保证 API 自足）；
		// main 注册的 applyConfig 也会再应用一次，结果一致。
		if !a.cfg.Pool.SetAccountProxy(uid, strings.TrimSpace(b.Proxy)) {
			writeErr(w, http.StatusNotFound, "not_found", "uid not in pool")
			return
		}
		log.Printf("admin: account %s proxy updated (persist=%v)", uid, persist)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "proxy": maskProxy(b.Proxy)})

	default:
		writeErr(w, http.StatusNotFound, "unknown_action", action)
	}
}

// ---------------------------------------------------------------------------
// 代理连通性测试
// ---------------------------------------------------------------------------

func (a *Admin) proxyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	var b struct {
		Proxy   string `json:"proxy"`
		EchoURL string `json:"echo_url"` // 可选，默认用 api.ipify.org 看出口 IP
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&b)
	proxyURL := strings.TrimSpace(b.Proxy)
	if proxyURL == "" {
		c := a.cfg.Manager.Get()
		proxyURL = c.Proxy.Default
	}
	echo := strings.TrimSpace(b.EchoURL)
	if echo == "" {
		echo = "https://api.ipify.org?format=json"
	}
	if _, err := upstream.ParseProxyURL(proxyURL); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_proxy", err.Error())
		return
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, echo, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	resp, err := upstream.DoWithProxy(proxyURL, req)
	elapsed := time.Since(start)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "proxy": maskProxy(proxyURL), "latency_ms": elapsed.Milliseconds(), "error": err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         resp.StatusCode < 400,
		"status":     resp.StatusCode,
		"proxy":      maskProxy(proxyURL),
		"latency_ms": elapsed.Milliseconds(),
		"body":       strings.TrimSpace(string(raw)),
	})
}

// ---------------------------------------------------------------------------
// 签到
// ---------------------------------------------------------------------------

func (a *Admin) claimsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	writeJSON(w, http.StatusOK, claimOverview(a.cfg))
}

func (a *Admin) claimsRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	if a.cfg.ClaimNow == nil {
		writeErr(w, http.StatusServiceUnavailable, "not_enabled", "claim scheduler not enabled")
		return
	}
	a.cfg.ClaimNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "triggered": true})
}

// ---------------------------------------------------------------------------
// 单账号签到 / 积分查询
// ---------------------------------------------------------------------------

// accountClaim /admin/api/accounts/{uid}/claim 对单个账号执行签到。
func (a *Admin) accountClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	uid := uidFrom(r.URL.Path, "/admin/api/accounts/")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing uid")
		return
	}
	acct := a.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "not_found", "uid not in pool")
		return
	}
	if acct.NeedsRefresh(30 * time.Minute) {
		if err := a.cfg.Upstream.RefreshToken(acct); err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				a.cfg.Pool.Disable(uid, "session dead")
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"ok": false, "uid": uid, "error": err.Error()})
			return
		}
		_ = acct.SaveAtomic()
	}
	m, err := a.cfg.Upstream.DailyClaim(acct)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "uid": uid, "error": err.Error()})
		return
	}
	if a.cfg.Claims != nil {
		a.cfg.Claims.Record(uid, true, summarizeClaim(m))
	}
	log.Printf("admin: manual claim uid=%s ok", uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "message": summarizeClaim(m)})
}

// accountRewards /admin/api/accounts/{uid}/rewards 查询账号积分/奖励。
func (a *Admin) accountRewards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	uid := uidFrom(r.URL.Path, "/admin/api/rewards/")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing uid")
		return
	}
	acct := a.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "not_found", "uid not in pool")
		return
	}
	if acct.NeedsRefresh(30 * time.Minute) {
		if err := a.cfg.Upstream.RefreshToken(acct); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"ok": false, "uid": uid, "error": err.Error()})
			return
		}
		_ = acct.SaveAtomic()
	}
	m, err := a.cfg.Upstream.Rewards(acct)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "uid": uid, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "rewards": m})
}

// uidFrom 从路径里截取 uid（去掉 prefix 与可能的 action 段）。
func uidFrom(path, prefix string) string {
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// summarizeClaim 提取签到响应摘要。
func summarizeClaim(m map[string]any) string {
	if d, ok := m["data"].(map[string]any); ok {
		m = d
	}
	for _, k := range []string{"message", "msg", "reward", "credit", "credits", "points"} {
		if v, ok := m[k]; ok {
			raw, _ := json.Marshal(v)
			s := string(raw)
			if len(s) > 200 {
				s = s[:200]
			}
			return s
		}
	}
	return "ok"
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}
