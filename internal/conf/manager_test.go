package conf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyPatchPartial(t *testing.T) {
	c := Default()
	c.Safety.DailyLimit = 100
	c.Proxy.Default = "http://old:8080"

	// 只改 proxy.default，其余保持
	var patch map[string]any
	raw := `{"proxy":{"default":"http://new:8080"}}`
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyPatch(patch); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if c.Proxy.Default != "http://new:8080" {
		t.Fatalf("proxy.default not updated: %q", c.Proxy.Default)
	}
	if c.Safety.DailyLimit != 100 {
		t.Fatalf("unrelated field must be preserved, got %d", c.Safety.DailyLimit)
	}
	if c.Listen != ":7864" {
		t.Fatalf("listen must be preserved, got %q", c.Listen)
	}
}

func TestApplyPatchNestedByAccount(t *testing.T) {
	c := Default()
	c.Proxy.ByAccount = map[string]string{"acc1": "http://a:1"}
	var patch map[string]any
	_ = json.Unmarshal([]byte(`{"proxy":{"by_account":{"acc2":"http://b:2"}}}`), &patch)
	if err := c.ApplyPatch(patch); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if c.Proxy.ByAccount["acc1"] != "http://a:1" || c.Proxy.ByAccount["acc2"] != "http://b:2" {
		t.Fatalf("by_account merge wrong: %v", c.Proxy.ByAccount)
	}
}

func TestApplyPatchRejectsBadDuration(t *testing.T) {
	c := Default()
	var patch map[string]any
	_ = json.Unmarshal([]byte(`{"cooldown":{"hard_credit":"not-a-duration"}}`), &patch)
	if err := c.ApplyPatch(patch); err == nil {
		t.Fatal("invalid duration should be rejected")
	}
}

// TestManagerUpdateHotApply 修改后 Runtime 立即变化，且回调被触发一次。
func TestManagerUpdateHotApply(t *testing.T) {
	c := Default()
	m := NewManager(c, "")
	calls := 0
	var seen *Config
	m.OnApply(func(c *Config) { calls++; seen = c })

	if m.Runtime().MaxRotate != 3 {
		t.Fatalf("initial max_rotate=%d", m.Runtime().MaxRotate)
	}
	if err := m.Update(func(c *Config) error {
		c.Safety.MaxRotate = 7
		c.Safety.JitterMax = "500ms"
		return nil
	}, false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 1 || seen == nil || seen.Safety.MaxRotate != 7 {
		t.Fatalf("callback not invoked correctly: calls=%d seen=%+v", calls, seen)
	}
	rt := m.Runtime()
	if rt.MaxRotate != 7 || rt.JitterMax != 500*time.Millisecond {
		t.Fatalf("runtime not hot-updated: %+v", rt)
	}
}

// TestManagerUpdateRollbackOnError mutate 返回错误时配置必须保持原样。
func TestManagerUpdateRollbackOnError(t *testing.T) {
	m := NewManager(Default(), "")
	before := m.Get().Safety.MaxRotate
	if err := m.Update(func(c *Config) error {
		c.Safety.MaxRotate = 99
		return os.ErrInvalid
	}, false); err == nil {
		t.Fatal("expected error")
	}
	if m.Get().Safety.MaxRotate != before {
		t.Fatalf("config mutated despite error: %d", m.Get().Safety.MaxRotate)
	}
}

// TestManagerPersistAndReload 保存后能从磁盘重新加载回来。
func TestManagerPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	m := NewManager(Default(), path)

	if err := m.Update(func(c *Config) error {
		c.Safety.Concurrency = 5
		c.Schedule.ClaimHours = []int{9, 21}
		return nil
	}, true); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	// 破坏内存状态后从磁盘恢复
	_ = m.Update(func(c *Config) error {
		c.Safety.Concurrency = 1
		c.Schedule.ClaimHours = nil
		return nil
	}, false)
	if err := m.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := m.Get()
	if got.Safety.Concurrency != 5 || len(got.Schedule.ClaimHours) != 2 {
		t.Fatalf("reload mismatch: concurrency=%d hours=%v", got.Safety.Concurrency, got.Schedule.ClaimHours)
	}
}

// TestManagerReloadNoPath 未指定配置文件时 Reload 应报错而不是 panic。
func TestManagerReloadNoPath(t *testing.T) {
	m := NewManager(Default(), "")
	if err := m.Reload(); err == nil {
		t.Fatal("expected error when no config path")
	}
}
