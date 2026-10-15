// Package scheduler 定时任务：token keepalive + 每日签到领积分。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"sync"
	"time"

	"phanthycode2api/internal/claims"
	"phanthycode2api/internal/pool"
	"phanthycode2api/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	KeepaliveHours []int // 默认 [22]
	ClaimHours     []int // 每日签到小时（本地时间），空 = 关闭签到
	Claims         *claims.Store
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	mu             sync.RWMutex
	keepaliveHours []int
	claimHours     []int
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	s := &Scheduler{
		cfg:            cfg,
		keepaliveHours: append([]int(nil), cfg.KeepaliveHours...),
		claimHours:     append([]int(nil), cfg.ClaimHours...),
	}
	return s
}

// SetHours 运行时修改 keepalive / 签到时间（面板热更新用）。
func (s *Scheduler) SetHours(keepalive, claim []int) {
	s.mu.Lock()
	s.keepaliveHours = append([]int(nil), keepalive...)
	s.claimHours = append([]int(nil), claim...)
	s.mu.Unlock()
}

// hours 读取当前生效时间。
func (s *Scheduler) hours() (keepalive, claim []int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keepaliveHours, s.claimHours
}

// ClaimHours 当前签到小时（空 = 关闭签到）。
func (s *Scheduler) ClaimHours() []int {
	_, c := s.hours()
	return append([]int(nil), c...)
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// nextClaimFire 每日签到触发时间：整点 + 0-15 分钟随机抖动，
// 避免所有账号在整点扎堆请求（拟人 + 避开上游峰值）。
func nextClaimFire(now time.Time, hours []int) time.Time {
	base := nextFire(now, hours)
	return base.Add(time.Duration(rand.Intn(16)) * time.Minute)
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	// 启动时立即执行一次 keepalive（保证服务起来后账号立即可用）
	log.Printf("scheduler: running initial keepalive")
	s.RunKeepaliveNow()

	_, claimHours := s.hours()
	if len(claimHours) > 0 {
		log.Printf("scheduler: running initial claim check (claim_hours=%v)", claimHours)
		s.RunClaimNow()
	}

	for {
		kaHours, claimHours := s.hours()
		nextKA := nextFire(time.Now(), kaHours)
		var nextClaim time.Time
		claimOn := len(claimHours) > 0
		if claimOn {
			nextClaim = nextClaimFire(time.Now(), claimHours)
		}
		wait := nextKA
		if claimOn && nextClaim.Before(wait) {
			wait = nextClaim
		}
		timer := time.NewTimer(time.Until(wait))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			now := time.Now()
			if claimOn && !now.Before(nextClaim) {
				s.RunClaimNow()
			}
			if !now.Before(nextKA) {
				s.RunKeepaliveNow()
			}
		}
	}
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		// 仅在 token 接近过期时刷新
		if !a.NeedsRefresh(30 * time.Minute) {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "session dead")
			}
			continue
		}
		// 确保 api_key 有效
		if a.APIKey == "" {
			if err := s.cfg.Upstream.EnsureAPIKey(a); err != nil {
				log.Printf("keepalive %s ensure_api_key: %v", st.UID, err)
			}
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
		log.Printf("keepalive %s: token refreshed", st.UID)
	}
}

// RunClaimNow 对所有可用账号执行每日签到；今天已成功签到的跳过（幂等）。
// 签到失败不冷却账号（与 chat 错误分开），只记录结果。
func (s *Scheduler) RunClaimNow() {
	if s.cfg.Claims == nil {
		return
	}
	okCnt, failCnt, skipCnt := 0, 0, 0
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if s.cfg.Claims.ClaimedToday(st.UID) {
			skipCnt++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		// token 临期先刷新（签到也要有效凭证）
		if a.NeedsRefresh(30 * time.Minute) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("claim %s: refresh failed: %v", st.UID, err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					s.cfg.Pool.Disable(st.UID, "session dead")
				}
				s.cfg.Claims.Record(st.UID, false, "refresh: "+err.Error())
				failCnt++
				continue
			}
			if err := a.SaveAtomic(); err != nil {
				log.Printf("claim %s save: %v", st.UID, err)
			}
		}
		resp, err := s.cfg.Upstream.DailyClaim(a)
		if err != nil {
			log.Printf("claim %s: %v", st.UID, err)
			s.cfg.Claims.Record(st.UID, false, err.Error())
			failCnt++
			continue
		}
		msg := summarize(resp)
		s.cfg.Claims.Record(st.UID, true, msg)
		okCnt++
		log.Printf("claim %s: ok %s", st.UID, msg)
	}
	log.Printf("claim: done (ok=%d fail=%d skipped=%d)", okCnt, failCnt, skipCnt)
}

// summarize 从签到响应提取可读摘要（message/积分等字段），失败退化为 "ok"。
func summarize(m map[string]any) string {
	if d, ok := m["data"].(map[string]any); ok {
		m = d
	}
	for _, k := range []string{"message", "msg", "reward", "credit", "credits", "points"} {
		if v, ok := m[k]; ok {
			return truncateAny(v)
		}
	}
	return "ok"
}

func truncateAny(v any) string {
	s, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return "ok"
		}
		s = string(b)
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
