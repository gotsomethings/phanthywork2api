// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
//
// 账号安全相关行为（详见 chatCompletions / attempt）：
//   - 命中风控：立即禁用该账号，并且【不换号重试】，直接把错误返回给客户端。
//     理由：风控通常作用于「会话/账号」维度，换号继续试探会把其他账号一起拖下水。
//   - 每账号并发槽位 + 每日请求上限，超出即跳过（换下一个 healthy 账号）。
//   - 每次派发前随机抖动，避免固定节奏被上游识别为机器人。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"phanthycode2api/internal/auth"
	"phanthycode2api/internal/claims"
	"phanthycode2api/internal/conf"
	"phanthycode2api/internal/pool"
	"phanthycode2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	APIKey   string // 空 = 不鉴权

	// Runtime 返回可热更新的运行参数快照（并发/抖动/冷却/风控开关/换号次数等）。
	// 每个请求现场取值，面板改动立即对后续请求生效，无需重启。
	Runtime func() *conf.Runtime

	// ---- 每日签到 ----
	Claims   *claims.Store // 签到记录（nil = 未启用）
	ClaimNow func()        // 手动触发一次签到（nil = 不支持）
}

// defaultRuntime 未注入 Runtime 时的兜底参数。
func defaultRuntime() *conf.Runtime {
	return &conf.Runtime{
		MaxRotate:    3,
		RefreshSkew:  10 * time.Minute,
		BlockOnRisk:  true,
		HardCooldown: 12 * time.Hour,
		SoftCooldown: 60 * time.Second,
		ErrThreshold: 3,
		ErrCooldown:  10 * time.Minute,
	}
}

// rt 取当前运行参数快照。
func (h *Handler) rt() *conf.Runtime {
	if h.cfg.Runtime == nil {
		return defaultRuntime()
	}
	if rt := h.cfg.Runtime(); rt != nil {
		return rt
	}
	return defaultRuntime()
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /claims", h.withAuth(h.claimsStatus))
	h.mux.HandleFunc("POST /claims/run", h.withAuth(h.claimsRun))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// claimsStatus 返回各账号今日签到结果（脱敏）。
func (h *Handler) claimsStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Claims == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"today":   time.Now().Format("2006-01-02"),
		"records": h.cfg.Claims.Snapshot(7),
	})
}

// claimsRun 手动触发一次签到（调试/补签用）。
func (h *Handler) claimsRun(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ClaimNow == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "claim not enabled"})
		return
	}
	h.cfg.ClaimNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// 静态模型表（客户端可见的商业命名）。
var staticModels = []map[string]any{
	{"id": "DeepSeek-V4", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "gpt-5.6-sol", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "gpt-5.6-terra", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "gpt-5.6-luna", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "gpt-5.5", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "Claude Opus 4.8", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "Claude Opus 4.7", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "Claude Sonnet 4.6", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "Kimi K3", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "Kimi-k2.7-code", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "Kimi K2.6", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "GLM 5.2", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
	{"id": "GLM-5.1", "object": "model", "created": 1753600000, "owned_by": "phanthy", "context_length": 200000},
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   staticModels,
	})
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	// 探测是否流式
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	rt := h.rt() // 本请求使用的运行参数快照（面板热更新后对新请求生效）
	tried := map[string]bool{}
	var lastErr error
	riskBlocked := false

	for i := 0; i < rt.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		// 闸门 1：每日请求上限（超限跳过该账号）
		if !h.cfg.Pool.ReserveDaily(acct.UID) {
			log.Printf("gate uid=%s: daily limit reached, skip", acct.UID)
			continue
		}
		// 闸门 2：并发槽位（占满则跳过，不排队堆积）
		if !h.cfg.Pool.AcquireSlot(acct.UID, rt.SlotWait) {
			log.Printf("gate uid=%s: concurrency full, skip", acct.UID)
			continue
		}

		served, risk, aerr := h.attempt(w, rt, acct, body, peek.Stream, peek.Model)
		h.cfg.Pool.ReleaseSlot(acct.UID)

		if served {
			return
		}
		lastErr = aerr
		if risk {
			// 风控熔断：禁用该账号；默认停止换号重试（避免把风险扩散到其他账号）
			riskBlocked = rt.BlockOnRisk
			if riskBlocked {
				break
			}
		}
	}

	if riskBlocked {
		msg := "account risk-controlled; request blocked to avoid spreading risk"
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		writeOpenAIError(w, http.StatusForbidden, "account_risk_control", msg)
		return
	}
	msg := "all accounts unavailable (cooling/disabled/limited)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// attempt 用指定账号发一次请求。
// 返回 (是否已响应客户端, 是否命中风控, 错误)。
func (h *Handler) attempt(w http.ResponseWriter, rt *conf.Runtime, acct *auth.Auth, body []byte, stream bool, model string) (served bool, risk bool, err error) {
	// 风控闸门 3：随机抖动，打散请求节奏（拟人化）
	if rt.JitterMax > 0 {
		d := time.Duration(rand.Int63n(int64(rt.JitterMax)))
		time.Sleep(d)
	}

	// token 临近过期 → 先 refresh（失败冷却换号）
	if acct.NeedsRefresh(rt.RefreshSkew) {
		if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				h.cfg.Pool.Disable(acct.UID, "refresh session dead")
			} else if errors.As(err, &ue) && ue.Kind == upstream.ErrRiskControl {
				h.cfg.Pool.Disable(acct.UID, "risk control on refresh")
				return false, true, err
			} else {
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, rt.ErrCooldown, "refresh: "+err.Error())
			}
			return false, false, err
		}
		_ = acct.SaveAtomic()
	}

	// 确保 api_key 可用（失败不阻塞，ChatStream 会用 access_token 兜底）
	if acct.APIKey == "" {
		if err := h.cfg.Upstream.EnsureAPIKey(acct); err != nil {
			log.Printf("ensure_api_key uid=%s: %v (will try access_token)", acct.UID, err)
		}
	}

	// 准备请求体（OpenAI → Anthropic 转换）
	anthroBody := upstream.PrepareBody(body)

	rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, anthroBody)
	if terr != nil {
		h.cfg.Pool.NoteError(acct.UID, rt.ErrThreshold, rt.ErrCooldown)
		return false, false, terr
	}
	if status >= 400 {
		kind := upstream.Classify(status, string(respBody))
		switch kind {
		case upstream.ErrRiskControl:
			// 熔断：禁用账号 + 不换号重试
			h.cfg.Pool.Disable(acct.UID, "risk control (http "+strconv.Itoa(status)+")")
			log.Printf("risk_control uid=%s: disabled, http=%d", acct.UID, status)
			return false, true, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
		case upstream.ErrHardCredit:
			h.cfg.Pool.Cooldown(acct.UID, pool.CoolHard, rt.HardCooldown, "积分不足")
			return false, false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
		case upstream.ErrSoftRate:
			h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, rt.SoftCooldown, "429 rate limit")
			return false, false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
		case upstream.ErrSessionDead:
			h.cfg.Pool.Disable(acct.UID, "session dead")
			return false, false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
		case upstream.ErrNotFound:
			h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, rt.SoftCooldown, "upstream 404")
			return false, false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
		default:
			h.cfg.Pool.NoteError(acct.UID, rt.ErrThreshold, rt.ErrCooldown)
			return false, false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
		}
	}
	defer rc.Close()
	h.cfg.Pool.NoteSuccess(acct.UID)

	if stream {
		if err := upstream.Stream(w, rc, model); err != nil {
			log.Printf("stream error: %v", err)
		}
		return true, false, nil
	}
	resp, aerr := upstream.Aggregate(rc, model)
	if aerr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", aerr.Error())
		return true, false, aerr
	}
	writeJSON(w, http.StatusOK, resp)
	return true, false, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
