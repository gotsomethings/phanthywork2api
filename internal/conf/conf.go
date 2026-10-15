// Package conf 配置定义与加载（JSON 文件 + P2A_* 环境变量覆盖）。
//
// 配置从 cmd/server 下沉到本包，是为了让 admin 管理面板、pool/upstream/scheduler
// 都能共享同一份可热重载的配置（见 manager.go）。
package conf

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen     string `json:"listen"`      // ":7864"
	APIKey     string `json:"api_key"`     // 空 = 不鉴权
	AuthDir    string `json:"auth_dir"`    // ./auths
	StateFile  string `json:"state_file"`  // ./data/state.json
	ClaimsFile string `json:"claims_file"` // ./data/claims.json
	BaseURL    string `json:"base_url"`    // https://code.phanthy.com

	Cooldown struct {
		HardCredit  string `json:"hard_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 默认 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		ClaimHours     []int `json:"claim_hours"`     // 每日签到小时，空 = 关闭
	} `json:"schedule"`

	// ---- 代理（一号一 IP 是账号安全的第一道防线）----
	Proxy struct {
		// Default 全局默认代理 URL，如 http://user:pass@host:port 或 socks5://host:port
		Default string `json:"default"`
		// ByAccount 账号级代理映射：uid（或凭证文件名去扩展名）→ 代理 URL
		// 与 Resin 等代理池配合时，认证用户名写成 Platform.Account:token 即可固定出口 IP。
		ByAccount map[string]string `json:"by_account"`
	} `json:"proxy"`

	// ---- 账号安全加固 ----
	Safety struct {
		DailyLimit      int    `json:"daily_limit"`           // 每账号每日请求上限，0 = 不限
		Concurrency     int    `json:"concurrency"`           // 每账号并发上限，0 = 不限
		SlotWait        string `json:"slot_wait"`             // 等槽位最长时间 "0s"=不等待
		JitterMax       string `json:"jitter_max"`            // 派发前随机抖动上限 "300ms"，空=关闭
		RefreshSkew     string `json:"refresh_skew"`          // token 提前刷新窗口 "10m"
		MaxRotate       int    `json:"max_rotate"`            // 单请求最多换号次数
		BlockOnRiskCtrl bool   `json:"block_on_risk_control"` // 命中风控是否阻断请求
	} `json:"safety"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 默认 600（含流式读取总时长）
		// 官方对齐：可覆盖 UA / anthropic-version / beta / 额外头
		UserAgent        string            `json:"user_agent"`
		AnthropicVersion string            `json:"anthropic_version"`
		Beta             string            `json:"beta"`
		ExtraHeaders     map[string]string `json:"extra_headers"`
	} `json:"upstream"`

	// 解析后（不序列化）
	HardCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`
	SlotWaitDur    time.Duration `json:"-"`
	JitterMaxDur   time.Duration `json:"-"`
	RefreshSkewDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:     ":7864",
		APIKey:     "",
		AuthDir:    "./auths",
		StateFile:  "./data/state.json",
		ClaimsFile: "./data/claims.json",
		BaseURL:    "https://code.phanthy.com",
	}
	c.Cooldown.HardCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.ClaimHours = []int{8}
	c.Proxy.ByAccount = map[string]string{}
	c.Safety.DailyLimit = 0
	c.Safety.Concurrency = 1
	c.Safety.SlotWait = "0s"
	c.Safety.JitterMax = "300ms"
	c.Safety.RefreshSkew = "10m"
	c.Safety.MaxRotate = 3
	c.Safety.BlockOnRiskCtrl = true
	c.Upstream.TimeoutSeconds = 600
	c.Upstream.ExtraHeaders = map[string]string{}
	return c
}

// Load 从文件读（path 为空则只用默认值 + 环境变量），再用 P2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Clone 深拷贝，供热重载的「改副本 → 校验 → 原子替换」使用。
func (c *Config) Clone() *Config {
	cp := *c
	cp.Schedule.KeepaliveHours = append([]int(nil), c.Schedule.KeepaliveHours...)
	cp.Schedule.ClaimHours = append([]int(nil), c.Schedule.ClaimHours...)
	if c.Proxy.ByAccount != nil {
		cp.Proxy.ByAccount = map[string]string{}
		for k, v := range c.Proxy.ByAccount {
			cp.Proxy.ByAccount[k] = v
		}
	}
	if c.Upstream.ExtraHeaders != nil {
		cp.Upstream.ExtraHeaders = map[string]string{}
		for k, v := range c.Upstream.ExtraHeaders {
			cp.Upstream.ExtraHeaders[k] = v
		}
	}
	return &cp
}

func applyEnv(c *Config) {
	if v := os.Getenv("P2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("P2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("P2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("P2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("P2A_CLAIMS_FILE"); v != "" {
		c.ClaimsFile = v
	}
	if v := os.Getenv("P2A_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("P2A_HARD_CREDIT"); v != "" {
		c.Cooldown.HardCredit = v
	}
	if v := os.Getenv("P2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("P2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("P2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("P2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	// ---- 代理 ----
	if v := os.Getenv("P2A_PROXY_URL"); v != "" {
		c.Proxy.Default = v
	}
	if v := os.Getenv("P2A_PROXY_BY_ACCOUNT"); v != "" {
		if c.Proxy.ByAccount == nil {
			c.Proxy.ByAccount = map[string]string{}
		}
		for _, kv := range strings.Split(v, ",") {
			kv = strings.TrimSpace(kv)
			if i := strings.Index(kv, "="); i > 0 {
				c.Proxy.ByAccount[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
			}
		}
	}
	// ---- 每日签到 ----
	if v := os.Getenv("P2A_CLAIM_HOURS"); v != "" {
		if v == "off" || v == "none" || v == "-1" {
			c.Schedule.ClaimHours = nil
		} else {
			hours := []int{}
			for _, s := range strings.Split(v, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 && n <= 23 {
					hours = append(hours, n)
				}
			}
			c.Schedule.ClaimHours = hours
		}
	}
	// ---- 安全加固 ----
	if v := os.Getenv("P2A_DAILY_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Safety.DailyLimit = n
		}
	}
	if v := os.Getenv("P2A_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Safety.Concurrency = n
		}
	}
	if v := os.Getenv("P2A_SLOT_WAIT"); v != "" {
		c.Safety.SlotWait = v
	}
	if v := os.Getenv("P2A_JITTER_MAX"); v != "" {
		c.Safety.JitterMax = v
	}
	if v := os.Getenv("P2A_REFRESH_SKEW"); v != "" {
		c.Safety.RefreshSkew = v
	}
	if v := os.Getenv("P2A_MAX_ROTATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Safety.MaxRotate = n
		}
	}
	// ---- 官方头对齐 ----
	if v := os.Getenv("P2A_UA"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("P2A_ANTHROPIC_VERSION"); v != "" {
		c.Upstream.AnthropicVersion = v
	}
	if v := os.Getenv("P2A_BETA"); v != "" {
		c.Upstream.Beta = v
	}
	if v := os.Getenv("P2A_EXTRA_HEADERS"); v != "" {
		if c.Upstream.ExtraHeaders == nil {
			c.Upstream.ExtraHeaders = map[string]string{}
		}
		for _, kv := range strings.Split(v, ",") {
			kv = strings.TrimSpace(kv)
			if i := strings.Index(kv, "="); i > 0 {
				c.Upstream.ExtraHeaders[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
			}
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.HardCreditDur, err = time.ParseDuration(c.Cooldown.HardCredit); err != nil {
		return fmt.Errorf("cooldown.hard_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 600
	}
	if c.Safety.SlotWait != "" {
		if c.SlotWaitDur, err = time.ParseDuration(c.Safety.SlotWait); err != nil {
			return fmt.Errorf("safety.slot_wait: %w", err)
		}
	}
	if c.Safety.JitterMax != "" {
		if c.JitterMaxDur, err = time.ParseDuration(c.Safety.JitterMax); err != nil {
			return fmt.Errorf("safety.jitter_max: %w", err)
		}
	}
	if c.Safety.RefreshSkew != "" {
		if c.RefreshSkewDur, err = time.ParseDuration(c.Safety.RefreshSkew); err != nil {
			return fmt.Errorf("safety.refresh_skew: %w", err)
		}
	}
	if c.RefreshSkewDur <= 0 {
		c.RefreshSkewDur = 10 * time.Minute
	}
	if c.Safety.MaxRotate <= 0 {
		c.Safety.MaxRotate = 3
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://code.phanthy.com"
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	hours := make([]int, 0, len(c.Schedule.ClaimHours))
	for _, h := range c.Schedule.ClaimHours {
		if h >= 0 && h <= 23 {
			hours = append(hours, h)
		}
	}
	c.Schedule.ClaimHours = hours
	return nil
}

// JSONDoc 导出可编辑字段的 map（面板读取/合并 patch 用）。
func (c *Config) JSONDoc() map[string]any { return c.toJSONDoc() }

// ApplyPatch 把 patch 深度合并进配置（只覆盖 patch 中出现的字段），再 normalize 校验。
// 用于面板的局部更新：{"proxy":{"default":"http://..."}} 这样只改一项。
func (c *Config) ApplyPatch(patch map[string]any) error {
	base := c.toJSONDoc()
	deepMerge(base, patch)
	raw, err := json.Marshal(base)
	if err != nil {
		return err
	}
	// 反序列化进已有结构：patch 未涉及的字段保持原值
	if err := json.Unmarshal(raw, c); err != nil {
		return fmt.Errorf("apply patch: %w", err)
	}
	return c.normalize()
}

// deepMerge 把 src 深度合并进 dst（map 递归，其余直接覆盖）。
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				deepMerge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// SaveJSON 原子写回配置文件（面板「保存并生效」用）。
func (c *Config) SaveJSON(path string) error {
	if path == "" {
		return fmt.Errorf("no config path configured")
	}
	raw, err := json.MarshalIndent(c.toJSONDoc(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// toJSONDoc 去掉解析后的只读字段，只留用户可编辑项。
func (c *Config) toJSONDoc() map[string]any {
	return map[string]any{
		"listen":      c.Listen,
		"api_key":     c.APIKey,
		"auth_dir":    c.AuthDir,
		"state_file":  c.StateFile,
		"claims_file": c.ClaimsFile,
		"base_url":    c.BaseURL,
		"cooldown": map[string]any{
			"hard_credit":   c.Cooldown.HardCredit,
			"soft_rate":     c.Cooldown.SoftRate,
			"err_threshold": c.Cooldown.ErrThresh,
			"err_cooldown":  c.Cooldown.ErrCooldown,
		},
		"schedule": map[string]any{
			"keepalive_hours": c.Schedule.KeepaliveHours,
			"claim_hours":     c.Schedule.ClaimHours,
		},
		"proxy": map[string]any{
			"default":    c.Proxy.Default,
			"by_account": c.Proxy.ByAccount,
		},
		"safety": map[string]any{
			"daily_limit":           c.Safety.DailyLimit,
			"concurrency":           c.Safety.Concurrency,
			"slot_wait":             c.Safety.SlotWait,
			"jitter_max":            c.Safety.JitterMax,
			"refresh_skew":          c.Safety.RefreshSkew,
			"max_rotate":            c.Safety.MaxRotate,
			"block_on_risk_control": c.Safety.BlockOnRiskCtrl,
		},
		"upstream": map[string]any{
			"timeout_seconds":   c.Upstream.TimeoutSeconds,
			"user_agent":        c.Upstream.UserAgent,
			"anthropic_version": c.Upstream.AnthropicVersion,
			"beta":              c.Upstream.Beta,
			"extra_headers":     c.Upstream.ExtraHeaders,
		},
	}
}
