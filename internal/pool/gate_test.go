package pool

import (
	"testing"
	"time"

	"phanthycode2api/internal/auth"
)

func newPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: "a1", AccessToken: "t1"})
	p.Add(&auth.Auth{UID: "a2", AccessToken: "t2"})
	return p
}

// TestReserveDailyLimit 每日上限生效后 PickExcluding 不再返回该账号。
func TestReserveDailyLimit(t *testing.T) {
	p := newPool(t)
	p.SetLimits(2, 0)
	for i := 0; i < 2; i++ {
		if !p.ReserveDaily("a1") {
			t.Fatalf("reserve #%d should succeed", i+1)
		}
	}
	if p.ReserveDaily("a1") {
		t.Fatal("3rd reserve should be blocked by daily limit")
	}
	got := p.PickExcluding(nil)
	if got == nil || got.UID != "a2" {
		t.Fatalf("expect pick a2 after a1 exhausted, got %v", got)
	}
	if !p.ReserveDaily("a2") {
		t.Fatal("reserve on other account should still work")
	}
}

// TestConcurrencySlot 并发槽位占满后非阻塞获取失败，释放后恢复。
func TestConcurrencySlot(t *testing.T) {
	p := newPool(t)
	p.SetLimits(0, 1)
	if !p.AcquireSlot("a1", 0) {
		t.Fatal("first acquire should succeed")
	}
	if p.AcquireSlot("a1", 0) {
		t.Fatal("second acquire should fail (concurrency=1)")
	}
	p.ReleaseSlot("a1")
	if !p.AcquireSlot("a1", 0) {
		t.Fatal("acquire after release should succeed")
	}
}

// TestCooldownBackoff 连续软限流冷却时长指数递增并封顶 1h。
func TestCooldownBackoff(t *testing.T) {
	p := newPool(t)
	base := time.Minute
	p.Cooldown("a1", CoolSoft, base, "429")
	st, _ := p.Status("a1")
	first := st.Until
	p.Cooldown("a1", CoolSoft, base, "429")
	st, _ = p.Status("a1")
	if !st.Until.After(first) {
		t.Fatalf("2nd cooldown should be longer: %v vs %v", st.Until, first)
	}
	for i := 0; i < 20; i++ {
		p.Cooldown("a1", CoolSoft, base, "429")
	}
	st, _ = p.Status("a1")
	if d := time.Until(st.Until); d > time.Hour+time.Minute {
		t.Fatalf("cooldown should cap at 1h, got %v", d)
	}
}

// TestPickSpreadsLoad 挑号应把流量摊开：已用次数多的账号让位给没用过的。
func TestPickSpreadsLoad(t *testing.T) {
	p := newPool(t)
	p.SetLimits(0, 0)
	// a1 已消耗 3 次
	for i := 0; i < 3; i++ {
		p.ReserveDaily("a1")
	}
	got := p.Pick()
	if got == nil || got.UID != "a2" {
		t.Fatalf("should pick least-used a2, got %v", got)
	}
	p.ReserveDaily("a2")
	p.ReserveDaily("a2")
	// 现在 a1=3 a2=2 → 仍选 a2
	if got := p.Pick(); got == nil || got.UID != "a2" {
		t.Fatalf("should keep picking a2 until it catches up, got %v", got)
	}
	p.ReserveDaily("a2")
	// a2=3 = a1 → 回到字典序 a1
	if got := p.Pick(); got == nil || got.UID != "a1" {
		t.Fatalf("tie should resolve to a1, got %v", got)
	}
}

// TestDisablePersists 禁用后不再被挑中。
func TestDisablePersists(t *testing.T) {
	p := newPool(t)
	p.Disable("a1", "risk control")
	got := p.PickExcluding(nil)
	if got == nil || got.UID != "a2" {
		t.Fatalf("disabled account must not be picked, got %v", got)
	}
	st, _ := p.Status("a1")
	if !st.Disabled || st.Reason != "risk control" {
		t.Fatalf("status mismatch: %+v", st)
	}
}
