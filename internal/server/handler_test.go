package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"phanthycode2api/internal/auth"
	"phanthycode2api/internal/conf"
	"phanthycode2api/internal/pool"
	"phanthycode2api/internal/upstream"
)

// newTestUpstream 起一个假上游，body 由 handler 决定。
func newTestUpstream(t *testing.T, h http.HandlerFunc) (*upstream.Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return upstream.New(ts.URL), ts
}

func newTestAccount(uid string) *auth.Auth {
	return &auth.Auth{
		UID:          uid,
		AccessToken:  "at-" + uid,
		RefreshToken: "rt-" + uid,
		ExpiresAt:    time.Now().Add(24 * time.Hour).Unix(), // 不触发 refresh
		APIKey:       "sk-" + uid,
	}
}

// testRuntime 测试用运行参数（风控阻断开启、无抖动，保证测试快速稳定）。
func testRuntime() *conf.Runtime {
	return &conf.Runtime{
		MaxRotate:    3,
		BlockOnRisk:  true,
		RefreshSkew:  10 * time.Minute,
		ErrThreshold: 3,
		ErrCooldown:  10 * time.Minute,
		HardCooldown: 12 * time.Hour,
		SoftCooldown: 60 * time.Second,
	}
}

func doChat(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestRiskControlDisablesAccountAndBlocks 命中风控：账号被禁用，且请求被阻断（403），
// 不得换其他账号继续重试。
func TestRiskControlDisablesAccountAndBlocks(t *testing.T) {
	up, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"账号异常，已被风控"}}`))
	})
	p := pool.New("")
	a1 := newTestAccount("a_risk")
	a2 := newTestAccount("b_healthy")
	p.Add(a1)
	p.Add(a2)

	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Runtime:  testRuntime,
	})

	w := doChat(h, `{"model":"DeepSeek-V4","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 on risk control, got %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if got := out["error"].(map[string]any)["code"]; got != "account_risk_control" {
		t.Fatalf("want code=account_risk_control, got %v", got)
	}
	// a1 被禁用
	if st, _ := p.Status("a_risk"); !st.Disabled {
		t.Fatalf("risk account must be disabled, got %+v", st)
	}
	// a2 未被牵连（没有被拿来重试）
	if st, _ := p.Status("b_healthy"); st.Disabled {
		t.Fatalf("other account must NOT be disabled: %+v", st)
	}
}

// TestSoftRateCooldown 429 只冷却、不禁用，且会换号重试直到无号可用。
func TestSoftRateCooldown(t *testing.T) {
	up, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	})
	p := pool.New("")
	p.Add(newTestAccount("r1"))
	p.Add(newTestAccount("r2"))

	h := NewHandler(Config{Pool: p, Upstream: up, Runtime: testRuntime})
	w := doChat(h, `{"model":"DeepSeek-V4","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when all accounts rate-limited, got %d", w.Code)
	}
	for _, uid := range []string{"r1", "r2"} {
		st, _ := p.Status(uid)
		if st.Disabled {
			t.Fatalf("%s should be cooling, not disabled", uid)
		}
		if !st.Cooling {
			t.Fatalf("%s should be cooling after 429", uid)
		}
	}
}

// TestDailyLimitSkip 达到每日上限的账号会被跳过，请求落到其他账号。
func TestDailyLimitSkip(t *testing.T) {
	var hit string
	up, _ := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"账号异常"}}`))
	})
	_ = hit
	p := pool.New("")
	p.Add(newTestAccount("d1"))
	p.Add(newTestAccount("d2"))
	p.SetLimits(1, 0)
	// 先把 d1 的配额吃满
	if !p.ReserveDaily("d1") {
		t.Fatal("reserve d1 failed")
	}

	h := NewHandler(Config{Pool: p, Upstream: up, Runtime: testRuntime})
	w := doChat(h, `{"model":"DeepSeek-V4","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	// d1 配额已满不该被选中 → 被禁用的应是 d2
	if st, _ := p.Status("d1"); st.Disabled {
		t.Fatalf("daily-limited account must be skipped, but d1 was disabled")
	}
	if st, _ := p.Status("d2"); !st.Disabled {
		t.Fatalf("d2 should have been used and disabled, got %+v", st)
	}
}
