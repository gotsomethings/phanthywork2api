package upstream

import (
	"net/http"
	"testing"
)

// TestIsRiskControlStrong 强关键词必须命中，且与状态码无关（含 200 信封错误）。
func TestIsRiskControlStrong(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"cn_fengkong", 200, `{"error":{"message":"账号存在风控"}}`},
		{"cn_banned", 403, `{"message":"账号已被封禁"}`},
		{"en_risk_control", 429, `{"message":"risk control triggered"}`},
		{"en_suspended", 200, `{"message":"account suspended"}`},
		{"en_verify", 400, `{"message":"verification required"}`},
	}
	for _, c := range cases {
		if !isRiskControl(c.status, c.body) {
			t.Errorf("%s: expect risk control, got false", c.name)
		}
		if k := Classify(c.status, c.body); k != ErrRiskControl {
			t.Errorf("%s: Classify=%s, want risk_control", c.name, k)
		}
	}
}

// TestIsRiskControlWeak 弱关键词只在 403/429 下判定为风控，普通 200 不误杀。
func TestIsRiskControlWeak(t *testing.T) {
	if isRiskControl(http.StatusForbidden, `{"message":"forbidden"}`) != true {
		t.Error("403 forbidden should be risk control")
	}
	if isRiskControl(http.StatusTooManyRequests, `{"message":"abnormal"}`) != true {
		t.Error("429 abnormal should be risk control")
	}
	if isRiskControl(http.StatusOK, `{"message":"forbidden"}`) != false {
		t.Error("200 with 'forbidden' must NOT be risk control (avoid false disable)")
	}
	if isRiskControl(http.StatusBadRequest, `{"message":"abnormal"}`) != false {
		t.Error("400 with 'abnormal' must NOT be risk control")
	}
}

// TestClassifyPrecedence 风控优先级高于积分不足/会话失效等分类。
func TestClassifyPrecedence(t *testing.T) {
	body := `{"message":"insufficient credit, 账号异常"}`
	if k := Classify(http.StatusPaymentRequired, body); k != ErrRiskControl {
		t.Errorf("risk should win over hard_credit, got %s", k)
	}
}
