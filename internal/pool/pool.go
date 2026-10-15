// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中 API key 优先（有 api_key 优于无），再按 uid 排序。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"phanthycode2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 积分/余额不足 → 长冷却
	CoolSoft                 // 429 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	HasAPI   bool      `json:"has_api_key"`
	Cooling  bool      `json:"cooling"`
	Until    time.Time `json:"until,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Disabled bool      `json:"disabled"`
	ErrCount int       `json:"err_count,omitempty"`
	Daily    int       `json:"daily,omitempty"` // 今日已 dispatched 请求数（风控闸门）
}

type entry struct {
	a        *auth.Auth
	disabled bool
	reason   string
	until    time.Time
	errCount int

	// 风控闸门（运行时状态，不持久化）
	day        string        // daily 计数所属日期 "2006-01-02"
	daily      int           // 今日已放行请求数
	sem        chan struct{} // 并发信号量（惰性创建）
	coolRounds int           // 连续冷却轮数（指数退避）
}

// scaleDuration 指数退避：第 n 轮时长 = base * 2^(n-1)，封顶 max。
func scaleDuration(base time.Duration, rounds int, max time.Duration) time.Duration {
	if rounds <= 1 {
		return base
	}
	shift := rounds - 1
	if shift > 6 {
		shift = 6
	}
	d := base << shift
	if d > max {
		d = max
	}
	return d
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]struct {
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		Until    time.Time `json:"until,omitempty"`
	} `json:"accounts"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string

	// 风控闸门限额（0/负 = 不限制）
	dailyLimit  int // 每账号每日请求上限
	concurrency int // 每账号并发上限
}

// SetLimits 设置每账号风控闸门。dailyLimit<=0 不限；concurrency<=0 不限。
func (p *Pool) SetLimits(dailyLimit, concurrency int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyLimit = dailyLimit
	p.concurrency = concurrency
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a == nil || a.UID == "" {
		return
	}
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留冷却状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的剔除。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 返回 healthy 中 api_key 优先的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 同时跳过已达到每日请求上限的账号（风控闸门）。
//
// 排序优先级：当日已用次数少者优先 > 有 api_key 优先 > uid 字典序（稳定）。
// 按用量最少挑选可把流量摊到所有账号，降低单账号请求频率（风控第一敏感项），
// 避免旧实现「按 uid 固定顺序」把全部流量压在同一个号上。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	today := now.Format("2006-01-02")
	var bestUID string
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		// 每日限额（e.day != today 时计数已过期，视为 0）
		if p.dailyLimit > 0 && e.day == today && e.daily >= p.dailyLimit {
			continue
		}
		if best == nil {
			best, bestUID = e, uid
			continue
		}
		// 1) 当日用量少者优先
		eUsed, bestUsed := e.daily, best.daily
		if e.day != today {
			eUsed = 0
		}
		if best.day != today {
			bestUsed = 0
		}
		if eUsed != bestUsed {
			if eUsed < bestUsed {
				best, bestUID = e, uid
			}
			continue
		}
		// 2) 有 api_key 优先
		bHas := e.a.APIKey != ""
		bestHas := best.a.APIKey != ""
		if bHas != bestHas {
			if bHas {
				best, bestUID = e, uid
			}
			continue
		}
		// 3) uid 字典序（稳定）
		if uid < bestUID {
			best, bestUID = e, uid
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// Cooldown 冷却账号至 now+d。软限流/错误冷却按指数退避逐轮加长（封顶 1h），
// 积分不足维持固定长冷却。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		switch kind {
		case CoolSoft, CoolErr:
			e.coolRounds++
			d = scaleDuration(d, e.coolRounds, time.Hour)
		}
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 死亡），需人工重登。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// Enable 手动恢复账号：解除禁用并清空冷却/错误计数（面板「解禁」用）。
func (p *Pool) Enable(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.reason = ""
	e.errCount = 0
	e.coolRounds = 0
	p.saveLocked()
	return true
}

// ClearCooldown 只清冷却与错误计数，不动 disabled 状态。
func (p *Pool) ClearCooldown(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.until = time.Time{}
	if e.reason != "" && !e.disabled {
		e.reason = ""
	}
	e.errCount = 0
	e.coolRounds = 0
	p.saveLocked()
	return true
}

// SetAccountProxy 运行时修改某账号的代理（空串表示改用全局默认）。
func (p *Pool) SetAccountProxy(uid, proxyURL string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok || e.a == nil {
		return false
	}
	e.a.SetProxy(proxyURL) // 空串不写入，即回落全局默认
	return true
}

// AccountProxy 查询某账号当前生效的代理（未设置返回空串，表示走全局默认）。
func (p *Pool) AccountProxy(uid string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok || e.a == nil {
		return "", false
	}
	return e.a.ProxyURL(), true
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
// 连续触发时冷却时长指数退避（base * 2^n，封顶 8h），NoteSuccess 重置。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.coolRounds++
			e.until = time.Now().Add(scaleDuration(d, e.coolRounds, 8*time.Hour))
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数与退避轮次。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
		e.coolRounds = 0
	}
}

// ReserveDaily 请求放行前调用：计数 +1；达到每日上限返回 false（调用方应换号/拒绝）。
func (p *Pool) ReserveDaily(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	today := time.Now().Format("2006-01-02")
	if e.day != today {
		e.day = today
		e.daily = 0
	}
	if p.dailyLimit > 0 && e.daily >= p.dailyLimit {
		return false
	}
	e.daily++
	return true
}

// AcquireSlot 申请账号并发槽位；wait<=0 非阻塞，否则最多等 wait。
// 槽位用完返回 false（调用方应换号/拒绝，不要无限等待）。
func (p *Pool) AcquireSlot(uid string, wait time.Duration) bool {
	p.mu.Lock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.Unlock()
		return false
	}
	if e.sem == nil {
		cap := p.concurrency
		if cap <= 0 {
			cap = 1024
		}
		e.sem = make(chan struct{}, cap)
	}
	sem := e.sem
	p.mu.Unlock()
	if wait <= 0 {
		select {
		case sem <- struct{}{}:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case sem <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

// ReleaseSlot 释放并发槽位。与 AcquireSlot 成对调用（defer）。
func (p *Pool) ReleaseSlot(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok || e.sem == nil {
		return
	}
	select {
	case <-e.sem:
	default:
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:      uid,
		Nickname: e.a.Nickname,
		HasAPI:   e.a.APIKey != "",
		Cooling:  !e.until.IsZero() && now.Before(e.until),
		Until:    e.until,
		Reason:   e.reason,
		Disabled: e.disabled,
		ErrCount: e.errCount,
		Daily:    e.daily,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:        &auth.Auth{UID: uid},
			disabled: s.Disabled,
			reason:   s.Reason,
			until:    s.Until,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]struct {
		Disabled bool      `json:"disabled"`
		Reason   string    `json:"reason,omitempty"`
		Until    time.Time `json:"until,omitempty"`
	}{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = struct {
			Disabled bool      `json:"disabled"`
			Reason   string    `json:"reason,omitempty"`
			Until    time.Time `json:"until,omitempty"`
		}{
			Disabled: e.disabled,
			Reason:   e.reason,
			Until:    e.until,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
