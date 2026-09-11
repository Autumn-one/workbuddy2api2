package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/appconfig"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Service 把 workbuddy2api 网关内嵌在 GUI 进程里。
// 因为同进程，所以能随时启停、加完账号立即热加载，不必再拉起独立 exe。
type Service struct {
	mu sync.Mutex

	cfg    *appconfig.Config
	pool   *pool.Pool
	up     *upstream.Client
	sch    *scheduler.Scheduler
	sess   *session.Router
	srv    *http.Server
	ln     net.Listener
	cancel context.CancelFunc

	running   bool
	startedAt time.Time
	lastErr   string
	redisMode string

	// 事件回调，由 UI 层装配（可为 nil）。
	OnCheckin         func(uid string, status scheduler.CheckinStatus, detail string)
	OnKeepalive       func(uid string, ok bool, detail string)
	OnCreditRefresh   func(uid string, remain int64, err error)
	OnAccountsChanged func()
	// usageStore token 用量统计（账号×模型×日期）。由 GUI 装配时创建并注入 handler；
	// Service 持有引用供界面读取与退出时落盘。
	usageStore *server.TokenUsageStore

	// OnCreditsChanged 积分变动回调（GUI 落盘+刷新「积分记录」表格）。
	// 可能从任意 goroutine 调用，实现必须并发安全且非阻塞。
	OnCreditsChanged func(ch pool.CreditChange)
}

func NewService() *Service { return &Service{} }

func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Service) LastErr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Service) StartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startedAt
}

func (s *Service) RedisMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.redisMode
}

// ListenAddr 当前生效的监听地址（未启动时取配置值）。
func (s *Service) ListenAddr(cfg *appconfig.Config) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil {
		return s.cfg.Listen
	}
	if cfg != nil {
		return cfg.Listen
	}
	return ""
}

// Accounts 返回账号池快照；服务未启动时返回 nil。
func (s *Service) Accounts() []pool.Status {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.List()
}

// Upstream 返回上游客户端（供手动签到/查积分使用）；未启动返回 nil。
func (s *Service) Upstream() *upstream.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.up
}

// Scheduler 返回调度器（供手动刷新额度复用其串行 + 限流节奏）；未启动返回 nil。
func (s *Service) Scheduler() *scheduler.Scheduler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sch
}

// AuthByUID 返回账号凭证对象。
func (s *Service) AuthByUID(uid string) *auth.Auth {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.AuthByUID(uid)
}

// SetCredits 更新账号积分显示值（供手动/启动刷新额度用）。服务未启动时空操作。
func (s *Service) SetCredits(uid string, credits int64) {
	s.SetCreditsReason(uid, credits, "手动刷新")
}

// SetCreditsReason 与 SetCredits 相同，但显式标注变动来源（供积分历史归因）。
func (s *Service) SetCreditsReason(uid string, credits int64, reason string) {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.SetCreditsReason(uid, credits, reason)
}

// SetUsageStore 注入 token 用量统计（Start 之前调用）。
func (s *Service) SetUsageStore(st *server.TokenUsageStore) {
	s.mu.Lock()
	s.usageStore = st
	s.mu.Unlock()
}

// UsageStore 返回用量统计（未注入时为 nil）。
func (s *Service) UsageStore() *server.TokenUsageStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usageStore
}

// SetPriority 设置账号优先级（权重乘子，0 = 未设置）。
// 供 GUI 标记"一次性登录"账号，让它们优先被消耗。服务未启动时空操作。
func (s *Service) SetPriority(uid string, priority float64) {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.SetPriority(uid, priority)
}

// Start 按配置装配并启动服务（幂等）。
func (s *Service) Start(cfg *appconfig.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		s.lastErr = err.Error()
		return fmt.Errorf("加载账号目录失败: %w", err)
	}

	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)
	mode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		mode = "upstash"
	}

	p := pool.New(cfg.StateFile)
	p.SetStore(store)
	p.RestoreFromSnapshot()
	p.SyncToDir(auths)
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCreditChangeHook(s.OnCreditsChanged)

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints

	var sessRouter *session.Router
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore()
		sessRouter.StartGC()
	}

	sch := scheduler.New(scheduler.Config{
		Pool:                  p,
		Upstream:              up,
		CheckinHours:          cfg.Schedule.CheckinHours,
		KeepaliveHours:        cfg.Schedule.KeepaliveHours,
		CreditRefreshInterval: cfg.CreditRefreshDur,
		OnCheckin:             s.OnCheckin,
		OnKeepalive:           s.OnKeepalive,
		OnCreditRefresh:       s.OnCreditRefresh,
	})

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		UsageStore:   s.usageStore,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  func() int { return 0 },
		RedisMode:    mode,
		SoftCooldown: cfg.SoftRateDur,
	})

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		if sessRouter != nil {
			sessRouter.StopGC()
		}
		s.lastErr = err.Error()
		return fmt.Errorf("监听 %s 失败: %w", cfg.Listen, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go sch.Run(ctx)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	s.cfg, s.pool, s.up, s.sch, s.sess, s.srv, s.ln, s.cancel = cfg, p, up, sch, sessRouter, srv, ln, cancel
	s.redisMode = mode
	s.running = true
	s.startedAt = time.Now()
	s.lastErr = ""
	return nil
}

// Stop 优雅停止服务并落盘账号池状态（幂等）。
func (s *Service) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	p, srv, ln, cancel, sess := s.pool, s.srv, s.ln, s.cancel, s.sess
	s.running = false
	s.cfg, s.pool, s.up, s.sch, s.sess, s.srv, s.ln, s.cancel = nil, nil, nil, nil, nil, nil, nil, nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if sess != nil {
		sess.StopGC()
	}
	if srv != nil {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(cctx)
		c()
	}
	if ln != nil {
		_ = ln.Close()
	}
	if p != nil {
		p.Flush()
	}
}

// ReloadAccounts 重新扫描账号目录并同步进账号池。
// 登录新账号后调用即可生效，不必重启服务。
func (s *Service) ReloadAccounts() (int, error) {
	s.mu.Lock()
	p, cfg := s.pool, s.cfg
	s.mu.Unlock()
	if p == nil || cfg == nil {
		return 0, fmt.Errorf("服务未运行")
	}
	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		return 0, err
	}
	p.SyncToDir(auths)
	if s.OnAccountsChanged != nil {
		s.OnAccountsChanged()
	}
	return len(auths), nil
}

// Restart 用新配置重启（供改完端口/密钥后应用）。
func (s *Service) Restart(cfg *appconfig.Config) error {
	s.Stop()
	return s.Start(cfg)
}
