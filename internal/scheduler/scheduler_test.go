package scheduler

import (
	"testing"
	"time"
)

func TestNextFire(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 15, 9, 30, 0, 0, loc)

	got := nextFire(now, []int{8})
	want := time.Date(2026, 10, 16, 8, 0, 0, 0, loc) // 今天的 8 点已过 → 明天
	if !got.Equal(want) {
		t.Fatalf("nextFire(8) = %v, want %v", got, want)
	}

	got = nextFire(now, []int{8, 22})
	want = time.Date(2026, 10, 15, 22, 0, 0, 0, loc) // 最近的下一个整点
	if !got.Equal(want) {
		t.Fatalf("nextFire(8,22) = %v, want %v", got, want)
	}
}

// TestNextClaimFireJitter 签到时间带 0-15 分钟抖动，且落在整点之后。
func TestNextClaimFireJitter(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 10, 15, 7, 0, 0, 0, loc)
	base := time.Date(2026, 10, 15, 8, 0, 0, 0, loc)
	for i := 0; i < 50; i++ {
		got := nextClaimFire(now, []int{8})
		d := got.Sub(base)
		if d < 0 || d > 15*time.Minute {
			t.Fatalf("jitter out of range: %v", d)
		}
	}
}

// TestRunClaimWithNilStore 未启用签到存储时调用不得 panic。
func TestRunClaimWithNilStore(t *testing.T) {
	s := New(Config{})
	s.RunClaimNow()
}
