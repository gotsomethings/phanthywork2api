// Package cliui 提供与 cli2api 控制台完全一致的 HTTP API 契约，
// 但底层数据全部来自 phanthywork2api 自己的账号池与运行时。
//
// 目的：前端直接复用 cli2api 的编译产物（React/HeroUI 界面）而不改一行前端代码，
// 由本包负责字段映射，保证面板外观 100% 一致且数据为真实数据。
package cliui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"phanthycode2api/internal/auth"
	"phanthycode2api/internal/claims"
	"phanthycode2api/internal/conf"
	"phanthycode2api/internal/pool"
	"phanthycode2api/internal/server"
	"phanthycode2api/internal/upstream"
)

// Config 兼容层依赖。
type Config struct {
	Manager  *conf.Manager
	Pool     *pool.Pool
	Upstream *upstream.Client
	Claims   *claims.Store
	APIKey   string
	Version  string
	AuthDir  string

	// OnAccountAdded 新账号落盘后回调（入池 + 应用代理）。
	OnAccountAdded func(*auth.Auth)
	// OnAccountUpdated 账号属性变更后回调。
	OnAccountUpdated func()
}

// Handler 兼容层入口。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// New 构建。
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}

	// ---- 概览 ----
	h.mux.HandleFunc("/api/overview", h.withKey(h.handleOverview))
	h.mux.HandleFunc("/api/overview/summary", h.withKey(h.handleOverview))

	// ---- 账号 ----
	h.mux.HandleFunc("/api/accounts", h.withKey(h.handleAccounts))
	h.mux.HandleFunc("/api/accounts/", h.withKey(h.handleAccountByID))
	h.mux.HandleFunc("/api/accounts/import", h.withKey(h.handleAccountImport))

	// ---- 模型 ----
	h.mux.HandleFunc("/api/models", h.withKey(h.handleModels))
	h.mux.HandleFunc("/api/models/", h.withKey(h.handleModelSetting))

	// ---- 渠道 ----
	h.mux.HandleFunc("/api/providers", h.withKey(h.handleProviders))

	// ---- 密钥 ----
	h.mux.HandleFunc("/api/keys", h.withKey(h.handleKeys))
	h.mux.HandleFunc("/api/keys/", h.withKey(h.handleKeyByID))

	// ---- 日志 ----
	h.mux.HandleFunc("/api/logs/stats", h.withKey(h.handleLogsStats))
	h.mux.HandleFunc("/api/logs/requests", h.withKey(h.handleLogsRequests))
	h.mux.HandleFunc("/api/logs/requests/", h.withKey(h.handleLogRequestByID))
	h.mux.HandleFunc("/api/logs/runtime", h.withKey(h.handleRuntimeLogs))

	// ---- 系统 ----
	h.mux.HandleFunc("/api/system/settings", h.withKey(h.handleSystemSettings))
	h.mux.HandleFunc("/api/system/resources", h.withKey(h.handleSystemResources))
	h.mux.HandleFunc("/api/system/console-key", h.withKey(h.handleConsoleKey))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// withKey 与 cli2api 一致：配置了 api_key 时要求 Bearer。
func (h *Handler) withKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
				strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != h.cfg.APIKey {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"message": "Missing/invalid API key", "type": "api_error", "code": "invalid_api_key"},
				})
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error"},
	})
}

// uid -> 稳定短 id（满足前端归档键与 CheckinRecord.account_id 的一致性）。
func accountID(uid string) string {
	sum := sha256.Sum256([]byte("p2a:" + uid))
	return "acc_" + hex.EncodeToString(sum[:6])
}

var (
	idToUID = struct {
		sync.RWMutex
		m map[string]string
	}{m: map[string]string{}}
	uidToID = struct {
		sync.RWMutex
		m map[string]string
	}{m: map[string]string{}}
)

func registerAccountID(uid string) string {
	if uid == "" {
		return ""
	}
	uidToID.RLock()
	if id, ok := uidToID.m[uid]; ok {
		uidToID.RUnlock()
		return id
	}
	uidToID.RUnlock()

	uidToID.Lock()
	defer uidToID.Unlock()
	if id, ok := uidToID.m[uid]; ok {
		return id
	}
	id := accountID(uid)
	uidToID.m[uid] = id
	idToUID.Lock()
	idToUID.m[id] = uid
	idToUID.Unlock()
	return id
}

func resolveUID(id string) (string, bool) {
	idToUID.RLock()
	uid, ok := idToUID.m[id]
	idToUID.RUnlock()
	return uid, ok
}

// ---------------------------------------------------------------------------
// 概览
// ---------------------------------------------------------------------------

func (h *Handler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	c := h.cfg.Manager.Get()
	rt := h.cfg.Manager.Runtime()
	st := h.cfg.Pool.List()

	hello, cooling, disabled := 0, 0, 0
	accounts := make([]map[string]any, 0, len(st))
	for _, s := range st {
		switch {
		case s.Disabled:
			disabled++
		case s.Cooling:
			cooling++
		default:
			hello++
		}
		proxy, _ := h.cfg.Pool.AccountProxy(s.UID)
		accounts = append(accounts, h.accountView(s, proxy, c))
	}
	if !sort.SliceIsSorted(accounts, func(i, j int) bool {
		return accounts[i]["name"].(string) < accounts[j]["name"].(string)
	}) {
		sort.Slice(accounts, func(i, j int) bool {
			return accounts[i]["name"].(string) < accounts[j]["name"].(string)
		})
	}

	models := make([]map[string]any, 0, len(server.Models()))
	for _, m := range server.Models() {
		id, _ := m["id"].(string)
		ctxLen, _ := m["context_length"].(int)
		if ctxLen == 0 {
			if f, ok := m["context_length"].(float64); ok {
				ctxLen = int(f)
			}
		}
		models = append(models, map[string]any{
			"id":                     id,
			"display_name":           id,
			"provider":               "phanthy",
			"owned_by":               "phanthy",
			"native_model":           id,
			"context_length":         ctxLen,
			"default_context_length": ctxLen,
			"context_editable":       false,
			"free":                   true,
		})
	}

	base := publicBase(c, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"time":        time.Now().Format(time.RFC3339),
		"model_count": len(models),
		"models":      models,
		"accounts":    accounts,
		"proxy": map[string]any{
			"ok":                        true,
			"service":                   "phanthywork2api",
			"provider":                  "resin",
			"providers":                 proxyProviders(c),
			"cross_provider_model_pool": false,
			"port":                      c.Listen,
			"chat_url":                  base + "/v1/chat/completions",
		},
		"worker": map[string]any{
			"ok":            true,
			"hot":           hello > 0,
			"endpoint":      base + "/v1",
			"account_count": len(st),
			"ready_count":   hello,
			"hot_count":     hello,
			"cooling_count": cooling,
		},
		"routing": map[string]any{"strategy": "round-robin"},
		"auth":    map[string]any{"has_user_blob": false, "has_pat": false},
		"access": map[string]any{
			"openai_base_url":   base + "/v1",
			"chat_completions":  base + "/v1/chat/completions",
			"messages":          base + "/v1/messages",
			"responses":         base + "/v1/responses",
			"models":            base + "/v1/models",
			"health":            base + "/healthz",
		},
		"_p2a": map[string]any{
			"version":     h.cfg.Version,
			"disabled":    disabled,
			"listen":      c.Listen,
			"upstream":    c.BaseURL,
			"config_path": h.cfg.Manager.Path(),
			"timeout":     rt.Timeout.String(),
			"max_rotate":  rt.MaxRotate,
			"claim":       claimView(h.cfg),
		},
	})
}

func publicBase(c *conf.Config, r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = strings.TrimPrefix(c.Listen, ":")
		if !strings.Contains(host, ":") {
			host = host + ":" + strings.TrimPrefix(c.Listen, ":")
		}
	}
	return scheme + "://" + host
}

func proxyProviders(c *conf.Config) []string {
	if strings.TrimSpace(c.Proxy.Default) != "" || len(c.Proxy.ByAccount) > 0 {
		return []string{"resin"}
	}
	return []string{}
}

func claimView(cfg Config) map[string]any {
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
			for _, rec := range m {
				if rec.OK {
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

func (h *Handler) accountView(s pool.Status, proxy string, c *conf.Config) map[string]any {
	id := registerAccountID(s.UID)
	status := "ready"
	switch {
	case s.Disabled:
		status = "disabled"
	case s.Cooling:
		status = "cooling"
	}
	an := map[string]any{
		"id":            id,
		"provider":      "phanthy",
		"region":        "global",
		"name":          displayName(s),
		"remote_uid":    s.UID,
		"auth_type":     authType(s.HasAPI),
		"enabled":       !s.Disabled,
		"max_inflight":  1,
		"priority":      50,
		"proxy_url":     proxy,
		"status":        status,
		"ready":         !s.Disabled && !s.Cooling,
		"hot":           !s.Disabled && !s.Cooling,
		"in_flight":     0,
		"restarts":      0,
		"runtime_state": status,
		"kind":          "phanthy",
		"created_at":    time.Time{}.Format(time.RFC3339),
		"updated_at":    time.Now().Format(time.RFC3339),
		"last_error":    s.Reason,
	}
	if !s.Until.IsZero() {
		an["cooldown_until"] = s.Until.Format(time.RFC3339)
	} else {
		an["cooldown_until"] = nil
	}
	return an
}

func displayName(s pool.Status) string {
	if strings.TrimSpace(s.Nickname) != "" {
		return s.Nickname
	}
	return s.UID
}

func authType(hasAPI bool) string {
	if hasAPI {
		return "api_key"
	}
	return "oauth"
}

// ---------------------------------------------------------------------------
// 账号 CRUD
// ---------------------------------------------------------------------------

func (h *Handler) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		st := h.cfg.Pool.List()
		list := make([]map[string]any, 0, len(st))
		c := h.cfg.Manager.Get()
		for _, s := range st {
			proxy, _ := h.cfg.Pool.AccountProxy(s.UID)
			list = append(list, h.accountView(s, proxy, c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list})
	case http.MethodPost:
		// 面板“新建账号”只创建占位（无凭据），随后走 device 登录流程。
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var in struct {
			Name     string `json:"name"`
			ProxyURL string `json:"proxy_url"`
		}
		_ = json.Unmarshal(body, &in)
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = "新账号"
		}
		pendingName.Lock()
		pendingName.m[name] = true
		pendingName.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       accountID("pending:" + name + strconv.Itoa(int(time.Now().UnixNano()))),
			"name":     name,
			"provider": "phanthy",
			"pending":  true,
		})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

var pendingName = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func (h *Handler) handleAccountByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	if rest == "" {
		writeErr(w, http.StatusBadRequest, "account id required")
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// 未落盘的占位账号：login/device 需要它。
	uid, known := resolveUID(id)

	switch action {
	case "login", "device":
		h.deviceLogin(w, r)
		return
	case "refresh":
		h.accountRefresh(w, r, uid, known)
		return
	case "checkin":
		h.accountCheckin(w, r, uid, known)
		return
	case "checkins":
		h.accountCheckins(w, r, uid, known)
		return
	case "export":
		h.accountExport(w, r, uid, known)
		return
	}

	if !known {
		writeErr(w, http.StatusNotFound, "unknown account: "+id)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var in struct {
			Enabled  *bool   `json:"enabled"`
			Name     *string `json:"name"`
			ProxyURL *string `json:"proxy_url"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if in.Enabled != nil {
			if *in.Enabled {
				h.cfg.Pool.Enable(uid)
			} else {
				h.cfg.Pool.Disable(uid, "disabled from console")
			}
		}
		if in.ProxyURL != nil {
			p := strings.TrimSpace(*in.ProxyURL)
			if p == "" {
				h.cfg.Pool.SetAccountProxy(uid, "")
			} else {
				h.cfg.Pool.SetAccountProxy(uid, p)
			}
			if h.cfg.OnAccountUpdated != nil {
				h.cfg.OnAccountUpdated()
			}
		}
		st, _ := h.cfg.Pool.Status(uid)
		proxy, _ := h.cfg.Pool.AccountProxy(uid)
		writeJSON(w, http.StatusOK, h.accountView(st, proxy, h.cfg.Manager.Get()))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// deviceLogin 返回授权链接（第一步）；前端后续 POST .../login/callback 粘贴 code。
func (h *Handler) deviceLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	state, authURL, err := h.cfg.Upstream.StartLogin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pendingState.Lock()
	pendingState.m[state] = &pendingLoginInfo{at: time.Now()}
	pendingState.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "pending",
		"authUrl":  authURL,
		"auth_url": authURL,
		"state":    state,
		"message":  "在浏览器完成授权后粘贴回调 URL",
	})
}

type pendingLoginInfo struct {
	at time.Time
}

var pendingState = struct {
	sync.Mutex
	m map[string]*pendingLoginInfo
}{m: map[string]*pendingLoginInfo{}}

func (h *Handler) accountRefresh(w http.ResponseWriter, r *http.Request, uid string, known bool) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !known {
		writeErr(w, http.StatusNotFound, "unknown account")
		return
	}
	acct := h.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "account not in pool")
		return
	}
	if acct.NeedsRefresh(30 * time.Minute) {
		if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if acct.APIKey == "" {
		_ = h.cfg.Upstream.EnsureAPIKey(acct)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) accountCheckin(w http.ResponseWriter, r *http.Request, uid string, known bool) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !known {
		writeErr(w, http.StatusNotFound, "unknown account")
		return
	}
	acct := h.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "account not in pool")
		return
	}
	m, err := h.cfg.Upstream.DailyClaim(acct)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if h.cfg.Claims != nil {
		h.cfg.Claims.Record(uid, true, summarize(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": summarize(m)})
}

func (h *Handler) accountCheckins(w http.ResponseWriter, r *http.Request, uid string, known bool) {
	if !known {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{}})
		return
	}
	items := []map[string]any{}
	if h.cfg.Claims != nil {
		for day, m := range h.cfg.Claims.Snapshot(7) {
			for u2, rec := range m {
				if u2 != uid {
					continue
				}
				status := "success"
				if !rec.OK {
					status = "failed"
				}
				items = append(items, map[string]any{
					"id":         accountID(uid) + "-" + day,
					"account_id": accountID(uid),
					"status":     status,
					"message":    rec.Msg,
					"created_at": day + "T00:00:00Z",
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items})
}

func (h *Handler) accountExport(w http.ResponseWriter, r *http.Request, uid string, known bool) {
	if !known {
		writeErr(w, http.StatusNotFound, "unknown account")
		return
	}
	acct := h.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "account not in pool")
		return
	}
	proxy, _ := h.cfg.Pool.AccountProxy(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":       uid,
		"nickname":  acct.Nickname,
		"proxy_url": proxy,
		"auth_type": authType(acct.APIKey != ""),
	})
}

func (h *Handler) handleAccountImport(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, "import is not supported by phanthywork2api")
}

func summarize(m map[string]any) string {
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
// 模型 / 渠道
// ---------------------------------------------------------------------------

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	for _, m := range server.Models() {
		id, _ := m["id"].(string)
		ctxLen := 200000
		switch v := m["context_length"].(type) {
		case int:
			ctxLen = v
		case float64:
			ctxLen = int(v)
		}
		list = append(list, map[string]any{
			"id":                     id,
			"display_name":           id,
			"provider":               "phanthy",
			"owned_by":               "phanthy",
			"native_model":           id,
			"context_length":         ctxLen,
			"default_context_length": ctxLen,
			"context_editable":       false,
			"free":                   true,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (h *Handler) handleModelSetting(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, "model settings are managed by config.json")
}

func (h *Handler) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"data": []map[string]any{
			{
				"id":       "phanthy",
				"label":    "PhanthyCode",
				"runtime":  "phanthy-cli",
				"regions":  []map[string]any{{"id": "global", "label": "全局"}},
				"capabilities": map[string]any{
					"browser_login": true,
					"pat_login":     false,
					"import_export": false,
				},
				"default_region": "global",
			},
		},
	})
}

// ---------------------------------------------------------------------------
// 密钥（映射到 API Key）
// ---------------------------------------------------------------------------

func (h *Handler) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.keyRecords()})
	case http.MethodPost:
		writeErr(w, http.StatusNotImplemented, "create keys by editing config.json (api_key)")
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) handleKeyByID(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, "keys are managed via config.json")
}

func (h *Handler) keyRecords() []map[string]any {
	k := h.cfg.APIKey
	if k == "" {
		return []map[string]any{}
	}
	prefix := k
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	sum := sha256.Sum256([]byte(k))
	return []map[string]any{{
		"id":           "key_" + hex.EncodeToString(sum[:5]),
		"name":         "默认密钥",
		"prefix":       prefix,
		"providers":    []string{"phanthy"},
		"enabled":      true,
		"created_at":   time.Time{}.Format(time.RFC3339),
		"updated_at":   time.Now().Format(time.RFC3339),
	}}
}

func (h *Handler) handleConsoleKey(w http.ResponseWriter, r *http.Request) {
	k := h.cfg.APIKey
	prefix := ""
	if len(k) > 8 {
		prefix = k[:8]
	}
	writeJSON(w, http.StatusOK, map[string]any{"prefix": prefix, "hint": "在 config.json 的 api_key 字段中管理"})
}

// ---------------------------------------------------------------------------
// 日志（本服务暂无请求日志存储，返回空集保持界面可用）
// ---------------------------------------------------------------------------

func (h *Handler) handleLogsStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"window":  map[string]any{"from": now.Add(-24 * time.Hour).Format(time.RFC3339), "to": now.Format(time.RFC3339), "hours": 24},
		"totals":  map[string]any{"requests": 0, "ok": 0, "incomplete": 0, "error": 0, "canceled": 0, "streaming": 0, "success_rate": 0},
		"latency": map[string]any{"avg_ms": nil, "p50_ms": nil, "p95_ms": nil, "ttfb_avg_ms": nil},
		"tokens":  map[string]any{"prompt": 0, "completion": 0, "cache_read": 0, "total": 0},
		"status":  []any{},
		"errors":  []any{},
		"models":  []any{},
		"accounts": []any{},
		"providers": []any{},
		"series":  []any{},
	})
}

func (h *Handler) handleLogsRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": 0})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0, "limit": 50, "offset": 0})
}

func (h *Handler) handleLogRequestByID(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, "request log not found")
}

func (h *Handler) handleRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "count": 0, "total": 0, "limit": 100, "offset": 0})
}

// ---------------------------------------------------------------------------
// 系统
// ---------------------------------------------------------------------------

func (h *Handler) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	c := h.cfg.Manager.Get()
	view := map[string]any{
		"cross_provider_model_pool":   false,
		"checkin_disabled_accounts":   false,
		"proxy_url":                   c.Proxy.Default,
		"routing_strategy":            "round-robin",
		"workbuddy_checkin_time":      "08:00",
		"checkin_times":               map[string]string{},
		"timezone":                    "Asia/Shanghai",
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, view)
		return
	}
	// PATCH：仅支持代理与签到小时等可安全热改项。
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	var in struct {
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.Unmarshal(body, &in); err == nil && strings.TrimSpace(in.ProxyURL) != "" {
		_ = h.cfg.Manager.Update(func(cc *conf.Config) error {
			cc.Proxy.Default = strings.TrimSpace(in.ProxyURL)
			return nil
		}, true)
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) handleSystemResources(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	writeJSON(w, http.StatusOK, map[string]any{
		"sampled_at":     time.Now().Format(time.RFC3339),
		"server": map[string]any{
			"pid":         os.Getpid(),
			"rss_bytes":   int64(ms.Sys),
			"heap_bytes":  int64(ms.HeapAlloc),
			"goroutines":  runtime.NumGoroutine(),
			"cpu_percent": 0,
		},
		"workers":         []any{},
		"total_rss_bytes": int64(ms.Sys),
	})
}

// CompleteLogin 供面板“粘贴回调 URL”完成授权（由 admin 包调用）。
func (h *Handler) CompleteLogin(id, callbackURL, proxy, nickname string) (*auth.Auth, error) {
	uid, _ := resolveUID(id)
	state := ""
	pendingState.Lock()
	for s := range pendingState.m {
		state = s
		break
	}
	pendingState.Unlock()
	authDir := h.cfg.AuthDir
	if authDir == "" {
		authDir = h.cfg.Manager.Get().AuthDir
	}
	acct, err := h.cfg.Upstream.ExchangeLogin(state, callbackURL, authDir, proxy, nickname, uid)
	if err != nil {
		return nil, err
	}
	if h.cfg.OnAccountAdded != nil {
		h.cfg.OnAccountAdded(acct)
	}
	registerAccountID(acct.UID)
	return acct, nil
}

// PendingStates 返回当前待完成登录的 state 列表（调试/前端轮询用）。
func (h *Handler) PendingStates() []string {
	out := []string{}
	pendingState.Lock()
	for s := range pendingState.m {
		out = append(out, s)
	}
	pendingState.Unlock()
	return out
}
