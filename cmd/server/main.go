// Package main phanthycode2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务 + 管理面板。
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"phanthycode2api/internal/admin"
	"phanthycode2api/internal/auth"
	"phanthycode2api/internal/claims"
	"phanthycode2api/internal/conf"
	"phanthycode2api/internal/pool"
	"phanthycode2api/internal/scheduler"
	"phanthycode2api/internal/server"
	"phanthycode2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := conf.Load(*cfgPath)
	if err != nil {
		// 注意：用 errors.Is 而非 os.IsNotExist——后者不拆 fmt.Errorf("%w") 包装，
		// 会导致「无配置文件时用默认值启动」的兜底失效。
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = conf.Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}
	mgr := conf.NewManager(cfg, *cfgPath)

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	for _, a := range auths {
		p.Add(a)
	}

	up := upstream.New(cfg.BaseURL)

	// 签到记录存储
	var claimStore *claims.Store
	schCfg := scheduler.Config{
		Pool:           p,
		Upstream:       up,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ClaimHours:     cfg.Schedule.ClaimHours,
	}
	if len(cfg.Schedule.ClaimHours) > 0 {
		claimStore = claims.Load(cfg.ClaimsFile)
		schCfg.Claims = claimStore
	}
	sch := scheduler.New(schCfg)

	// ---- 配置热生效：面板/API 改配置后立即应用，不重启 ----
	mgr.OnApply(func(c *conf.Config) {
		applyConfig(c, p, up, sch, auths)
	})
	applyConfig(cfg, p, up, sch, auths)

	h := server.NewHandler(server.Config{
		Pool:     p,
		Upstream: up,
		APIKey:   cfg.APIKey,
		Runtime:  mgr.Runtime,
		Claims:   claimStore,
		ClaimNow: func() {
			go sch.RunClaimNow()
		},
	})

	adm := admin.New(admin.Config{
		Manager:  mgr,
		Pool:     p,
		Upstream: up,
		Claims:   claimStore,
		ClaimNow: func() { go sch.RunClaimNow() },
		APIKey:   cfg.APIKey,
		Version:  version,
	})

	// 面板与主 API 共用一个端口：/admin/* 走面板，其余走 OpenAI 兼容接口
	root := http.NewServeMux()
	root.Handle("/admin/", adm)
	root.Handle("/admin", http.RedirectHandler("/admin/", http.StatusFound))
	root.Handle("/", h)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	rt := mgr.Runtime()
	log.Printf("phanthycode2api %s listening on %s", version, cfg.Listen)
	log.Printf("  accounts=%d api_key=%v proxy=%s claim_hours=%v daily_limit=%d concurrency=%d timeout=%s",
		len(auths), cfg.APIKey != "", proxySummary(cfg), cfg.Schedule.ClaimHours,
		cfg.Safety.DailyLimit, cfg.Safety.Concurrency, rt.Timeout)
	log.Printf("  admin panel: http://<host>%s/admin/", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// version 构建版本（可用 -ldflags "-X main.version=x.y.z" 覆盖）。
var version = "1.1.0"

// applyConfig 把配置应用到各组件（启动时与热更新时共用）。
func applyConfig(c *conf.Config, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, auths []*auth.Auth) {
	p.SetLimits(c.Safety.DailyLimit, c.Safety.Concurrency)

	up.SetOpts(&upstream.Opts{
		AnthropicVersion: defStr(c.Upstream.AnthropicVersion, "2023-06-01"),
		BetaHeader:       defStr(c.Upstream.Beta, "oauth-2025-04-20"),
		UserAgent:        defStr(c.Upstream.UserAgent, "phanthycode2api/1.0"),
		ExtraHeaders:     c.Upstream.ExtraHeaders,
		DefaultProxy:     c.Proxy.Default,
		Timeout:          time.Duration(c.Upstream.TimeoutSeconds) * time.Second,
	})

	// 代理接线：账号级优先，其次全局默认。
	applyProxies(auths, c)

	sch.SetHours(c.Schedule.KeepaliveHours, c.Schedule.ClaimHours)
}

// applyProxies 把配置的代理写到账号凭证上。
// 优先级：proxy.by_account[uid] > proxy.by_account[文件名] > proxy.default。
func applyProxies(auths []*auth.Auth, cfg *conf.Config) {
	byFile := map[string]string{}
	for k, v := range cfg.Proxy.ByAccount {
		byFile[k] = v
	}
	for _, a := range auths {
		if pxy, ok := byFile[a.UID]; ok && pxy != "" {
			upstream.ForgetProxy(a.ProxyURL())
			a.SetProxy(pxy)
			continue
		}
		if a.FilePath != "" {
			base := strings.TrimSuffix(filepath.Base(a.FilePath), filepath.Ext(a.FilePath))
			if pxy, ok := byFile[base]; ok && pxy != "" {
				upstream.ForgetProxy(a.ProxyURL())
				a.SetProxy(pxy)
				continue
			}
		}
		// 未命中账号级映射：清空旧的账号级代理，回落全局默认
		if a.ProxyURL() != "" {
			upstream.ForgetProxy(a.ProxyURL())
			a.ClearProxy()
		}
	}
}

func proxySummary(c *conf.Config) string {
	if c.Proxy.Default == "" && len(c.Proxy.ByAccount) == 0 {
		return "off"
	}
	if len(c.Proxy.ByAccount) > 0 {
		return "per-account(" + itoa(len(c.Proxy.ByAccount)) + ")"
	}
	return "global"
}

func defStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return fmtInt(n)
}

func fmtInt(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
