package claims

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreClaimedTodayAndSnapshot(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "claims.json")
	s := Load(fp)
	if s.ClaimedToday("u1") {
		t.Fatal("fresh store should not claim u1 today")
	}
	s.Record("u1", true, "+10 credits")
	if !s.ClaimedToday("u1") {
		t.Fatal("u1 should be marked claimed today")
	}
	if s.ClaimedToday("u2") {
		t.Fatal("u2 should not be claimed")
	}
	// 失败记录不算已签到（下次调度会重试）
	s.Record("u2", false, "timeout")
	if s.ClaimedToday("u2") {
		t.Fatal("failed record must not count as claimed")
	}

	// 落盘后可重新加载
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("claims file not written: %v", err)
	}
	s2 := Load(fp)
	if !s2.ClaimedToday("u1") {
		t.Fatal("reloaded store lost u1 record")
	}
	snap := s2.Snapshot(7)
	if len(snap) != 1 {
		t.Fatalf("snapshot days=%d, want 1", len(snap))
	}
	if r := snap[todayKey()]["u1"]; !r.OK {
		t.Fatalf("u1 record should be ok: %+v", r)
	}
}

func todayKey() string {
	return nowDay()
}
