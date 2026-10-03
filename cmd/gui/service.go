package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/appconfig"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/proxy"
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
	// proxyReg 账号级出口代理分配表（nil = 未启用，全部直连）。
	proxyReg *proxy.Registry

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

// IsModelCooling 报告某账号的某模型是否处于 6004 冷却中。
// 供"一键检测"避开冷却中的账号（它们本来就不可用，探测只会把冷却误报成不可用）。
// 服务未启动或无该账号时返回 false。
func (s *Service) IsModelCooling(uid, model string) bool {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return false
	}
	return p.IsModelCooling(uid, model)
}

// ClearSuspectBan 手动解除账号的"疑似被上游拉黑"判定（清零计数并清冷却）。
// 不消耗积分——不需要发请求复检，只改本地状态。
func (s *Service) ClearSuspectBan(uid string) {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.ClearSuspectBan(uid)
}

// SetProxyRegistry 注入账号级代理分配表（Start 之前调用）；nil = 直连。
func (s *Service) SetProxyRegistry(reg *proxy.Registry) {
	s.mu.Lock()
	s.proxyReg = reg
	s.mu.Unlock()
}

// ProxyRegistry 返回代理分配表（未启用时为 nil）。
func (s *Service) ProxyRegistry() *proxy.Registry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyReg
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

// SetAccountActive 设置账号是否进入活跃号池（GUI 复选框：临时指定只用某几个账号）。
// 实时生效（下一个请求的 Pick 即跳过未勾选账号）；勾选状态持久化在 state.json。
// 服务未启动时空操作（账号表本来就空，无框可勾）。
func (s *Service) SetAccountActive(uid string, active bool) {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.SetActive(uid, active)
}

// SetActiveAccounts 精确设定活跃集（全选/反选/单框切换后的整体对齐）。
// activeUIDs 为空 = 无号可用（GUI 侧自行防呆/确认）。
func (s *Service) SetActiveAccounts(activeUIDs []string) {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.SetActiveSet(activeUIDs)
}

// Start 按配置装配并启动服务（幂等）。
func (s *Service) Start(cfg *appconfig.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}

	auths, keep, err := loadAuthsWithWarn(cfg.AuthDir)
	if err != nil {
		s.lastErr = err.Error()
		return fmt.Errorf("加载账号目录失败: %w", err)
	}
	log.Printf("已加载 %d 个账号（%s）", len(auths), cfg.AuthDir)

	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)
	mode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		mode = "upstash"
	}

	p := pool.New(cfg.StateFile)
	p.SetStore(store)
	p.RestoreFromSnapshot()
	p.SyncToDir(auths, keep)
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCreditChangeHook(s.OnCreditsChanged)

	up := upstream.New()
	// 账号级出口代理：每个账号走自己的本地 Clash 端口（独立出口 IP）。
	up.SetProxySelector(proxySelectorFor(s.proxyReg))
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
		Pool:                 p,
		Upstream:             up,
		UsageStore:           s.usageStore,
		MaxRotate:            cfg.UpstreamRotate.MaxRotate,
		APIKey:               cfg.APIKey,
		Session:              sessRouter,
		StickyCount:          func() int { return 0 },
		RedisMode:            mode,
		SoftCooldown:         cfg.SoftRateDur,
		ImageFootnoteDefault: cfg.ImageFootnoteDefault(),
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
	go authBackupLoop(ctx, cfg.AuthDir)
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
	auths, keep, err := loadAuthsWithWarn(cfg.AuthDir)
	if err != nil {
		return 0, err
	}
	p.SyncToDir(auths, keep)
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

// loadAuthsWithWarn 扫描凭证目录并返回 keepUIDs（文件在但解析失败的 UID 集合）。
// 坏文件逐条打告警日志——一次性登录凭证的损坏必须显性可见，
// 不能表现得像"账号被删了"一样无声消失。
func loadAuthsWithWarn(dir string) ([]*auth.Auth, map[string]bool, error) {
	auths, bad, err := auth.LoadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var keep map[string]bool
	for _, bf := range bad {
		uid := auth.UIDFromFileName(bf.Path)
		log.Printf("警告：凭证文件无法读取/解析，已跳过加载（文件仍在磁盘，不会被当作已删除）: %s: %v",
			bf.Path, bf.Err)
		if uid != "" {
			if keep == nil {
				keep = map[string]bool{}
			}
			keep[uid] = true
		}
	}
	return auths, keep, nil
}

// authBackupLoop 周期性给凭证目录做快照（SnapshotDir 内部指纹去重，
// 内容没变时只读一遍目录哈希，不写盘）。启动时先立即做一次。
func authBackupLoop(ctx context.Context, dir string) {
	snapshotOnce := func() {
		p, changed, err := auth.SnapshotDir(dir, 200)
		if err != nil {
			log.Printf("账号目录快照失败: %v", err)
		} else if changed {
			log.Printf("账号快照已更新: %s", p)
		}
	}
	snapshotOnce()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snapshotOnce()
		}
	}
}
