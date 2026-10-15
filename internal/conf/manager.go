package conf

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Runtime 每次请求都要读、且允许热更新的运行参数。
// 用 atomic.Pointer 整体替换，避免逐个字段加锁或产生数据竞争。
type Runtime struct {
	SlotWait     time.Duration // 等并发槽位最长时间
	JitterMax    time.Duration // 派发前随机抖动上限
	RefreshSkew  time.Duration // token 提前刷新窗口
	MaxRotate    int           // 单请求最多换号次数
	BlockOnRisk  bool          // 命中风控是否阻断整个请求
	HardCooldown time.Duration // 积分不足冷却
	SoftCooldown time.Duration // 429 冷却
	ErrThreshold int
	ErrCooldown  time.Duration
	Timeout      time.Duration // 上游超时
}

// RuntimeOf 从配置导出运行参数快照。
func RuntimeOf(c *Config) *Runtime {
	return &Runtime{
		SlotWait:     c.SlotWaitDur,
		JitterMax:    c.JitterMaxDur,
		RefreshSkew:  c.RefreshSkewDur,
		MaxRotate:    c.Safety.MaxRotate,
		BlockOnRisk:  c.Safety.BlockOnRiskCtrl,
		HardCooldown: c.HardCreditDur,
		SoftCooldown: c.SoftRateDur,
		ErrThreshold: c.Cooldown.ErrThresh,
		ErrCooldown:  c.ErrCooldownDur,
		Timeout:      time.Duration(c.Upstream.TimeoutSeconds) * time.Second,
	}
}

// Manager 持有当前配置，支持运行时读取与热更新。
//
// 更新流程：Clone → mutate → normalize 校验 → 原子替换 → 触发 onApply 回调 → 可选落盘。
// 校验失败则整份丢弃，绝不留下半改状态。
type Manager struct {
	mu      sync.RWMutex
	cfg     *Config
	path    string // 配置文件路径（空 = 不落盘）
	rt      atomic.Pointer[Runtime]
	onApply []func(*Config)
}

// NewManager 构建；path 非空时支持 Reload / 落盘。
func NewManager(c *Config, path string) *Manager {
	m := &Manager{cfg: c, path: path}
	m.rt.Store(RuntimeOf(c))
	return m
}

// OnApply 注册配置生效回调（由 main 接线到 pool/upstream/scheduler）。
func (m *Manager) OnApply(fn func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onApply = append(m.onApply, fn)
}

// Get 返回配置快照（拷贝，调用方可安全读取）。
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone()
}

// Runtime 返回当前运行参数（原子读，热路径安全）。
func (m *Manager) Runtime() *Runtime {
	if rt := m.rt.Load(); rt != nil {
		return rt
	}
	return RuntimeOf(m.Get())
}

// Path 配置文件路径。
func (m *Manager) Path() string { return m.path }

// Update 修改配置并热生效。mutate 返回错误则整份放弃。
// persist=true 时同时写回配置文件。
func (m *Manager) Update(mutate func(*Config) error, persist bool) error {
	if mutate == nil {
		return fmt.Errorf("nil mutate")
	}
	m.mu.Lock()
	next := m.cfg.Clone()
	m.mu.Unlock()

	if err := mutate(next); err != nil {
		return err
	}
	if err := next.normalize(); err != nil {
		return err
	}
	if persist && m.path != "" {
		if err := next.SaveJSON(m.path); err != nil {
			return fmt.Errorf("persist config: %w", err)
		}
	}
	m.mu.Lock()
	m.cfg = next
	cbs := append([]func(*Config){}, m.onApply...)
	m.mu.Unlock()
	m.rt.Store(RuntimeOf(next))
	for _, fn := range cbs {
		if fn != nil {
			fn(next)
		}
	}
	return nil
}

// Reload 从磁盘重新加载配置并热生效（忽略环境变量重放，以文件内容为准）。
func (m *Manager) Reload() error {
	if m.path == "" {
		return fmt.Errorf("no config path configured")
	}
	next, err := Load(m.path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg = next
	cbs := append([]func(*Config){}, m.onApply...)
	m.mu.Unlock()
	m.rt.Store(RuntimeOf(next))
	for _, fn := range cbs {
		if fn != nil {
			fn(next)
		}
	}
	return nil
}
