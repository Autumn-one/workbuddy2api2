// Package pool 账号池：单一状态机（健康/冷却/熔断）+ 在途租约 + 三因子加权挑选 + state.json 持久化。
//
// 每个账号只有三个正交状态维度：
//  1. 健康维度（唯一权威）：healthy = !disabled && !until 生效 && !breakerUntil 生效
//     - until：按错误类型的即时冷却（CoolSoft 429 / CoolHard 余额耗尽）
//     - breakerUntil：连续失败（fails）累计触发熔断的指数退避截止
//  2. 并发维度：inFlight（在途租约，运行态）
//  3. 统计维度：successCount / errTotal（累计，供成功率权重）/ lastUsed / lastSuccess / lastErr
//
// 挑选策略：healthy 账号中按三因子权重（credits 占比 ×10 + 闲置补偿 + 成功率 ×3）取 Top5，
// 再在 Top5 内按同一三因子权重加权随机（credits 只是权重的一个因子，不单独决定短名单）。
package pool

import (
	"encoding/json"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 冷却到次日 04:00（等签到恢复）
	CoolSoft                 // 429 → 短冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Credits  int64  `json:"credits"`
	// CreditsKnown 标记 credits 是否为上游可信值：false = 从未刷新过额度（未知，
	// 不能当作 0 参与求和/展示，否则总数会静默偏低）。
	CreditsKnown    bool      `json:"credits_known"`
	Cooling         bool      `json:"cooling"`
	CoolKind        string    `json:"cool_kind,omitempty"`
	CoolRemaining   int64     `json:"cool_remaining_sec,omitempty"`
	Until           time.Time `json:"until,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	Disabled        bool      `json:"disabled"`
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrTotal        int64     `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`

	// 运行态（不持久化）：在途请求数 + 熔断器状态。
	// Priority 账号优先级（权重乘子）：0 = 未设置。供 GUI 显示标记状态。
	Priority float64 `json:"priority,omitempty"`

	InFlight     int       `json:"in_flight"`
	InFlightPeak int       `json:"in_flight_peak"` // 近 peakWindow 内的在途峰值（"忙过"痕迹）
	PeakActive   bool      `json:"peak_active"`    // 峰值是否仍在可见窗口内
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}

// CreditChange 一次积分变动（观测用，不参与任何决策）。
//
// Old/New 是该账号积分的前后快照；Delta = New - Old。First 标记本次为该账号
// 首次拿到余额（此前无可信“旧值”，此时 Delta 语义不成立，调用方应展示为“首次获取”）。
// Reason 是变动来源（签到/自动刷新/手动刷新/未知），由写路径归因，不猜。
type CreditChange struct {
	UID    string    `json:"uid"`
	Old    int64     `json:"old"`
	New    int64     `json:"new"`
	Delta  int64     `json:"delta"`
	First  bool      `json:"first,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// CreditChangeFunc 积分变动回调（GUI 用来落盘+刷新表格）。可能从任意 goroutine 调用，
// 必须是并发安全的且不得阻塞（慢操作请自行异步化）。
type CreditChangeFunc func(CreditChange)

type entry struct {
	a            *auth.Auth
	credits      int64
	successCount int64     // 累计成功
	errTotal     int64     // 累计错误（供成功率权重 successRate = successCount/(successCount+errTotal)，不清零）
	lastErr      time.Time // 最近一次错误时间
	lastSuccess  time.Time // 最近一次成功时间
	coolKind     CoolKind
	until        time.Time // 冷却截止（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）
	disabled     bool
	reason       string
	lastUsed     time.Time // 最近被选中时刻（防并发撞号）

	// breakerUntil / fails / retryCount 为熔断器运行态（不持久化）。
	// fails 是唯一的"连续失败"计数器：任何错误喂入，达到 breakerThreshold 触发熔断（指数退避），
	// 跨入口累计，成功/熔断/统一复活时清零（保留 retryCount 驱动退避指数）。
	breakerUntil time.Time // 熔断截止（指数退避）
	fails        int       // 连续失败计数（熔断用，唯一权威）
	retryCount   int       // 已熔断次数（指数退避的指数）

	// inFlight 单账号在途请求数（运行态，不持久化）。用 atomic 避免 Pick 热路径拿写锁。
	inFlight atomic.Int64

	// creditsKnown 标记 credits 是否已从上游拿到过可信值。
	// 从未刷新过额度时 credits=0 只代表“未知”而非“真 0”，首次变动不参与涨跌归因。
	creditsKnown bool

	// modelCool 按模型冷却（运行态，不持久化）：6004 模型级限流用。
	// 键是【模型 ID】而非账号——6004 是模型级限制，同账号其他模型照常可用；
	// 账号级冷却会误伤其他模型（见 applyErrorPolicy 既有结论）。
	// 生命周期短（10min~60min），与 inFlight 同类不落盘；重启后从零开始，可接受。
	modelCool map[string]*modelCoolState

	// inFlightPeak / peakAt 为「忙闲痕迹」运行态（不持久化）：记录近 peakWindow 内的
	// 在途峰值与其最后一次统计时刻。短请求的 +1/−1 可能整体落在 GUI 采样间隔之间
	// （tick=1.5s），瞬时值看不到；峰值保留一段时间供界面展示"刚忙过"。
	// 纯读路径（statusOf）按 now-peakAt > peakWindow 判定过期，不回写。
	inFlightPeak atomic.Int64
	peakAt       atomic.Int64 // UnixNano；0 = 从未忙过

	// priority 账号优先级（权重乘子，持久化）。0 = 未设置 → 视为 1.0（不加权）。
	// 用途：用户手工标记"一次性登录"的账号（手机号一次性、失效后无法二次登录），
	// 给它更高权重以优先消耗掉额度，避免这类账号的额度被浪费。
	// 刻意做成【乘子】而非绝对优先：保持加权随机，使它在冷却/在途满时自动让位，
	// 不会被集中打爆而更快撞模型级限流。
	priority float64
}

// peakWindow 在途峰值的可见窗口：最后一次占用在途后的这段时间内，
// 界面仍展示该峰值，用于呈现"最近忙过"的痕迹。
const peakWindow = 10 * time.Second

// bumpPeak 抬升峰值至当前在途数，并刷新统计时刻。仅在 Acquire 成功后调用（写路径）。
func (e *entry) bumpPeak(now time.Time) {
	cur := e.inFlight.Load()
	for {
		old := e.inFlightPeak.Load()
		if cur <= old {
			break
		}
		if e.inFlightPeak.CompareAndSwap(old, cur) {
			break
		}
	}
	e.peakAt.Store(now.UnixNano())
}

// peakVisible 返回窗口内的在途峰值；超出 peakWindow 视为无痕迹（返回 0,false）。
func (e *entry) peakVisible(now time.Time) (int, bool) {
	nanos := e.peakAt.Load()
	if nanos == 0 {
		return 0, false
	}
	if now.Sub(time.Unix(0, nanos)) > peakWindow {
		return 0, false
	}
	return int(e.inFlightPeak.Load()), true
}

// healthy 报告账号当前是否可选（未禁用、未处于任一冷却/熔断期）。
func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	return true
}

// expiry 返回账号当前仍在生效的最近冷却/熔断截止时间（两个截止取较早者）；不在冷却期返回零值。
// 供全冷却兜底选取"最早到期"账号用。
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	return t
}

// fallbackKind 报告兜底账号属于哪一类冷却（soft：即时软冷却；breaker：熔断期）。
// 只对参与兜底的账号调用（CoolHard 已被 pickEarliestExpiryLocked 排除）。判定口径：
// 若熔断截止是当前生效的最近截止（含"仅有熔断无软冷却"），记为 breaker；否则记为 soft。
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			return "breaker"
		}
	}
	return "soft"
}

// stateAccount 单个账号的持久化状态（JSON tag 全小写下划线，向后兼容：缺字段零值）。
type stateAccount struct {
	Credits int64 `json:"credits"`
	// CreditsKnown 标记 credits 是否已从上游拿到过可信值。
	// 缺字段（旧文件）为 false：旧 state 的 credits 无法区分"真 0"与"从未刷过"。
	CreditsKnown bool      `json:"credits_known,omitempty"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind"`
	SuccessCount int64     `json:"success_count,omitempty"`
	// err_total 累计错误计数。旧版 err_count（连续错误）仍可读：加载时映射到 err_total，
	// 仅作一次性迁移，不再回写 err_count。
	ErrTotal    int64     `json:"err_total,omitempty"`
	ErrCount    int       `json:"err_count,omitempty"` // 兼容旧文件的迁移源，仅读取
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// Priority 账号优先级（权重乘子，用户手工标记一次性账号用）。
	// 缺字段（旧文件）为 0 = 未设置，行为与引入该功能前一致。
	Priority float64 `json:"priority,omitempty"`
}

// stateFile 持久化格式。
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval 后台落盘周期。
var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 12

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘

	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter

	// onCreditsChanged 积分变动回调；nil = 静默（不记录）。
	// 在持锁路径上调用，必须非阻塞。
	onCreditsChanged CreditChangeFunc

	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration

	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64

	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int

	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64

	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
// 与本地 state.json 并存，作启动恢复备份：快照比本地新才采用，否则本地优先。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// defaultIdle* 闲置补偿默认参数（claude-api selectWeightedRandom 参考口径）。
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("pool: 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	if localErr == nil && !localInfo.ModTime().After(snap.SavedAt) {
		// 快照不早于本地 → 采用快照。
		p.mu.Lock()
		p.applySnapshotLocked(snap)
		p.mu.Unlock()
		p.dirty.Store(true)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	log.Printf("pool: 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
}

// Acquire 为账号占一个在途名额；false 表示该账号已达上限（或不存在）。
// 必须在成功 Pick 后调用；调用方负责 defer Release。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	limit := p.maxInFlight
	p.mu.RUnlock()
	if !ok {
		return false
	}
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		e.bumpPeak(time.Now())
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			e.bumpPeak(time.Now())
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// startFlusher 每 flushInterval 检查 dirty 标志，有变更则 saveLocked 落盘；
// 顺带按固定周期清扫已过期的模型级冷却条目（防止（账号×模型）map 无限增长）。
func (p *Pool) startFlusher() {
	interval := flushInterval // 在启动 goroutine 前同步读取，避免与测试对 flushInterval 的恢复写竞争
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		sweep := time.NewTicker(modelCoolSweepInterval)
		defer sweep.Stop()
		for {
			select {
			case <-t.C:
				p.mu.Lock()
				if p.dirty.Swap(false) {
					p.saveLocked()
				}
				p.mu.Unlock()
			case now := <-sweep.C:
				p.mu.Lock()
				p.sweepModelCooldownsLocked(now)
				p.mu.Unlock()
			}
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按三因子权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool, model string) *auth.Auth {
	return p.pick(tried, model)
}

// pick 在 healthy 候选集中按三因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按三因子权重（weightOf）降序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
func (p *Pool) pick(tried map[string]bool, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	var cands []*entry
	modelAware := strings.TrimSpace(model) != "" // model 为空 = 不过滤（旧调用方行为不变）

	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if modelAware && e.modelCooling(model, now) {
			continue // 该账号对此模型正被 6004 冷却：跳过
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 && modelAware {
		// 模型级冷却把候选压空。两层兜底（按优先级）：
		//
		// 1) 半开探测：若有（账号×模型）冷却已过安静期，放行一次真实探测——
		//    成功即清冷却（走 NoteSuccess：一次真实成功是"上游已恢复"的最强信号），
		//    上游恢复后服务在几分钟内自愈，而不是干等 10m~15m 冷却截止
		//    （20:22 事故的教训）。
		// 2) 全部不可用 → 返回 nil 让 handler 直接 503 快速失败，
		//    比起再浪费一次注定失败的上游调用更诚实也更便宜。
		if a := p.pickHalfOpenLocked(model, now); a != nil {
			return a
		}
		return nil
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, now)
	}
	// top5 短名单按三因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿 + 成功率
	// 根本进不了短名单决策，低 credits 但高成功率/久置的账号会永远排不进 top5。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 权重只算一次：顶 5 截断要排序，若在 sort 比较器里现算 weightOf 会翻成 O(n log n) 次
	// 冗余浮点计算（46 账号约 500 次）。先做 O(n) 预计算，再按 (权重, uid) 排序。
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, len(cands))
	for i, e := range cands {
		ws[i] = weighted{e: e, w: p.weightOf(e, maxCredits, now)}
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	if len(cands) > 5 {
		cands = cands[:5]
	}

	eligible := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，维持发散且不 starve 任一候选。
		e = cands[0]
		for _, c := range cands[1:] {
			if c.lastUsed.Before(e.lastUsed) {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = top5 降序子集
	}
	e.lastUsed = time.Now()
	return e.a
}

// pickHalfOpenLocked 从处于"冷却中但安静期已过"的候选里按概率挑一个探测。
// 优先选【冷却剩余最短】的（最可能已恢复，探测成功率高）；
// 无候选或未命中概率时返回 nil。调用方需已持 p.mu。
func (p *Pool) pickHalfOpenLocked(model string, now time.Time) *auth.Auth {
	var cands []*entry
	for _, e := range p.byUID {
		if e.disabled {
			continue
		}
		// 与 HalfOpenAllowed 共用同一判定（entry.modelProbeWindow）
		if !e.modelProbeWindow(model, now) {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		return nil
	}
	// 先排序（剩余冷却最短者优先 = 最可能已恢复），再做判定与选取。
	// 顺序很重要：判定必须基于【确定的】候选序，否则会依赖 map 遍历的随机顺序
	// （审计缺陷：原先读 cands[0] 判定却在其后排序，混合场景下保底语义随机失效约 15%）。
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].modelCool[model].until.Before(cands[j].modelCool[model].until)
	})

	// 概率门只压【首次】探测：N 个账号同时进入半开窗口时防惊群（10% 概率放行）。
	// 只要有【任一】候选已探测过（probeFails>0）就豁免概率门——每过安静期必然放行
	// 一次探测，保证自愈下限不被概率门随机拖延。
	//
	// 用「任一」而非「最优」：保底语义应当与候选顺序无关（审计缺陷的核心）。
	alreadyProbed := false
	for _, c := range cands {
		if c.modelCool[model].probeFails > 0 {
			alreadyProbed = true
			break
		}
	}
	if !alreadyProbed {
		// 多个新半开候选同时放行会惊群 → 概率门只放其中一部分。
		// 随机源与 pickWeighted 同约定：randInt64N 仅供测试注入，生产走全局源。
		rnd := rand.Int64N
		if p.randInt64N != nil {
			rnd = p.randInt64N
		}
		const scale = 1_000_000
		if int64(halfOpenProbeRate*float64(scale)) <= rnd(scale) {
			return nil
		}
	}
	best := cands[0]
	best.lastUsed = now
	// 打标「已探测过」：后续过安静期必放行（豁免概率门），保证保底自愈节奏。
	// 布尔语义（封顶 1）：该标记只回答"探测过没有"，不承载次数统计，
	// 避免同一请求多轮 pick 反复打标导致无限增长。
	// 清零点：NoteSuccess（探测成功）。
	best.modelCool[model].probeFails = 1
	return best.a
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if e.disabled {
			continue // 禁用的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("pool: fallback_earliest_expiry uid=%s until=%s kind=%s", best.a.UID, best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读 p.maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(p.maxInFlight)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight + successRate × 3
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//	  - successRate = successCount/(successCount+errTotal)；无请求记录给 1.5（中性偏信任）
//
// credits 全 0 时仍按 idle+successRate 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}

	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.weightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}

	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的三因子权重。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0

	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}

	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}

	// 3. 成功率 ×3。
	totalReq := e.successCount + e.errTotal
	if totalReq > 0 {
		w += float64(e.successCount) / float64(totalReq) * 3
	} else {
		w += 1.5 // 无请求记录 → 中性偏信任
	}

	// 4. 账号优先级（权重乘子）。未设置（<=0）视为 1.0，行为与未引入该功能时一致。
	// 乘在总分上：让"高优先级 + 高积分/久置"的账号最优先被消耗。
	if e.priority > 0 {
		w *= e.priority
	}
	return w
}

// SetCreditChangeHook 注入积分变动回调（GUI 用它落盘+刷新表格）；nil = 关闭记录。
func (p *Pool) SetCreditChangeHook(fn CreditChangeFunc) {
	p.mu.Lock()
	p.onCreditsChanged = fn
	p.mu.Unlock()
}

// noteCreditsLocked 记录一次积分变动并触发回调。调用方必须已持有 p.mu。
//
// 注意：本函数只做观测，不改 credits——真实赋值已由调用方完成。
// 回调在持锁下调用，因此实现必须非阻塞（本项目的回调只做内存追加 + 异步落盘）。
func (p *Pool) noteCreditsLocked(e *entry, uid string, newCredits int64, reason string) {
	old := e.credits
	first := !e.creditsKnown
	e.creditsKnown = true

	if p.onCreditsChanged == nil {
		return
	}
	// 无变化不记录（除非首次获取，首次即使 0 也值得留一条“已知余额”锚点？
	// 不——首次 0 只是“未知”的另一种写法，记录它反而是噪声），故始终要求 old != new 或 first。
	if !first && old == newCredits {
		return
	}
	ch := CreditChange{
		UID:    uid,
		Old:    old,
		New:    newCredits,
		Delta:  newCredits - old,
		First:  first,
		Reason: reason,
		At:     time.Now(),
	}
	if first {
		// 首次拿到余额：delta 无参照系，显式归零避免谎报涨跌。
		ch.Delta = 0
	}
	p.onCreditsChanged(ch)
}

// modelCoolState 单个（账号×模型）对的模型级限流冷却状态（运行态）。
type modelCoolState struct {
	until      time.Time // 本次冷却截止
	lastHit    time.Time // 最近一次撞墙时刻
	backoffN   int       // 已连续撞墙次数（指数退避的指数；成功清零）
	probeFails int       // 半开是否已探测过（布尔语义 0/1；探测成功或普通成功时清零）
}

// 模型级限流冷却：起步 10 分钟，连续撞墙翻倍，封顶 15 分钟。
//
// 封顶从 60 分钟下调的依据：有半开探测兜底（见下），不需要靠长冷却挡子弹——
// 冷却进入安静期后就有概率被真实请求试探，上游恢复后几十秒内自愈；
// 长冷却反而拉长"上游已恢复但服务还在 503"的断供窗口（20:22 事故断供 9 分钟）。
//
// 半开探测（half-open）：冷却进入 halfOpenQuiet 安静期后，允许以
// halfOpenProbeRate 概率把冷却中的（账号×模型）作为候选发一次真实探测——
// 成功立即清冷却，失败维持冷却。没有它，上游对某模型的全局限流会把所有账号
// 先后推进长冷却（20:22 事故：6 账号全部冷却、服务断供 9 分钟），而上游实际
// 恢复远早于冷却截止。
const (
	maxModelCooldown = 15 * time.Minute

	// halfOpenQuiet 半开探测前的安静期：撞墙后先静默这么久再允许试探，
	// 避免刚限流就立刻重试（必然再撞）。取值远小于起步冷却 10 分钟。
	halfOpenQuiet = 2 * time.Minute

	// halfOpenProbeRate 半开探测概率：安静期过后每次 pick 的探测放行比例。
	// 概率而非必然：N 个账号同时半开时不会瞬间全部打上游；10% 配合
	// 生产请求频率意味着恢复后约几十秒内必被探测到。
	halfOpenProbeRate = 0.10
)

// modelCooldownFor 返回第 n 次连续撞墙应冷却的时长：base × 2^(n-1)，封顶 maxModelCooldown。
// base 为调用方传入的基础时长（生产用 5 分钟；参数化便于测试与调优）。
func modelCooldownFor(base time.Duration, n int) time.Duration {
	if base <= 0 {
		base = time.Minute
	}
	d := base
	for i := 1; i < n; i++ {
		d *= 2
		if d >= maxModelCooldown {
			return maxModelCooldown
		}
	}
	return d
}

// modelCooling 报告该 entry 对指定模型是否处于模型级冷却（调用方需已持锁或明确单协程语义；
// 本方法只读，不加锁，由 pick 的持锁路径与测试调用）。
func (e *entry) modelCooling(model string, now time.Time) bool {
	st, ok := e.modelCool[model]
	if !ok || st == nil {
		return false
	}
	return now.Before(st.until)
}

// NoteModelRateLimit 记录一次【账号×模型】撞上 6004，返回本次冷却时长。
//
// 冷却 = base × 2^(连续撞墙次数-1)，封顶 maxModelCooldown（15 分钟）。
// 同账号的其他模型、其他账号不受影响（模型级，不是账号级）。
// 返回 0 表示未记录：账号不存在/已禁用/入参非法。
//
// 成功（NoteSuccess）清零连续计数：账号有正常成功请求即恢复信任，
// 下次撞墙从第一档 5 分钟起步，避免一次偶发限流导致长期高冷却。
//
// 到期后【退避记忆保留】（条目在过期后才被清扫）：再撞墙从当前档起步。
// 否则每次到期就撞一次永远停在第一档，退避失去意义。
func (p *Pool) NoteModelRateLimit(uid, model string, base time.Duration) time.Duration {
	if uid == "" || strings.TrimSpace(model) == "" {
		return 0
	}
	if base <= 0 {
		base = time.Minute
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.disabled {
		return 0
	}
	now := time.Now()
	if e.modelCool == nil {
		e.modelCool = map[string]*modelCoolState{}
	}
	st, ok := e.modelCool[model]
	if !ok || st == nil {
		st = &modelCoolState{}
		e.modelCool[model] = st
	}
	// 连续计数：上次冷却尚未到期就又撞墙 → 连续；已过期后撞 → 视作新一轮，但仍从
	// 上次档位+1 起步（记忆保留），除非中间出现过成功（NoteSuccess 已清零）。
	if now.Before(st.until) {
		st.backoffN++
	} else {
		st.backoffN++
	}
	d := modelCooldownFor(base, st.backoffN)
	st.until = now.Add(d)
	st.lastHit = now
	p.dirty.Store(true)
	return d
}

// HalfOpenAllowed 报告该（账号×模型）当前是否允许作为【半开探测候选】：
// 冷却中（正常路径会被排除）且距上次撞墙已过安静期。无冷却状态时返回 true
// （此时不走半开路径，账号本来就可选）。
// 概率放行在 pick 内做（需要随机源），这里只做确定性判定。
func (p *Pool) HalfOpenAllowed(uid, model string) bool {
	if uid == "" || strings.TrimSpace(model) == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	st, ok := e.modelCool[model]
	if !ok || st == nil {
		return true // 无冷却 → 正常可选
	}
	if !time.Now().Before(st.until) {
		return true // 冷却已过期 → 正常可选，不算半开
	}
	return e.modelProbeWindow(model, time.Now()) // 与选号路径同一判定
}

// modelProbeWindow 报告该（账号×模型）是否处于【半开探测窗口】：
// 冷却尚未到期（正常路径会被排除）且距上次撞墙已过安静期。
// 这是半开判定的【唯一真源】：HalfOpenAllowed（对外查询/测试）与
// pickHalfOpenLocked（选号）共用，避免两份拷贝各自漂移。
// 调用方需已持锁（pick 持写锁，HalfOpenAllowed 持读锁）。
func (e *entry) modelProbeWindow(model string, now time.Time) bool {
	st, ok := e.modelCool[model]
	if !ok || st == nil {
		return false
	}
	if !now.Before(st.until) {
		return false // 已过期 → 属正常候选路径，不算半开
	}
	return now.Sub(st.lastHit) >= halfOpenQuiet
}

// IsModelCooling 报告该（账号×模型）是否处于模型级冷却中。
func (p *Pool) IsModelCooling(uid, model string) bool {
	return !p.ModelCooldownUntil(uid, model).IsZero() && time.Now().Before(p.ModelCooldownUntil(uid, model))
}

// ModelCooldownUntil 返回该（账号×模型）的冷却截止时间；未冷却返回零值。
func (p *Pool) ModelCooldownUntil(uid, model string) time.Time {
	if uid == "" || strings.TrimSpace(model) == "" {
		return time.Time{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}
	}
	st, ok := e.modelCool[model]
	if !ok {
		return time.Time{}
	}
	return st.until
}

// ExcludedByModelCooldown 选号候选过滤语义：该（账号×模型）冷却中则应被排除。
func (p *Pool) ExcludedByModelCooldown(uid, model string) bool {
	return p.IsModelCooling(uid, model)
}

// AdvanceModelCooldowns 测试专用：把所有模型冷却立即视为已到期。
// 一律置为「当前墙钟的过去」，保证 IsModelCooling/pick 的 now.Before(until) 为 false（已恢复）；
// 不接受调用方指定未来时刻（那会把 until 设到未来，语义就错了）。
// 只动 until、不动 backoffN——「退避记忆在到期后保留」正是被测语义。
func (p *Pool) AdvanceModelCooldowns(_ time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	past := time.Now().Add(-time.Second)
	for _, e := range p.byUID {
		for _, st := range e.modelCool {
			st.until = past
		}
	}
}

// modelCoolSweepInterval 模型级冷却条目的清扫周期。
const modelCoolSweepInterval = 5 * time.Minute

// sweepModelCooldownsLocked 清理早已过期的模型级冷却条目，防止 map 无限增长。
// 保留期 2 小时（> 封顶 60 分钟）：到期≠信任恢复，退避记忆要保留一段时间，
// 让"到期→再撞墙→升档"的退避链在跨窗口后仍然成立。
func (p *Pool) sweepModelCooldownsLocked(now time.Time) {
	const keep = 2 * time.Hour
	for _, e := range p.byUID {
		for m, st := range e.modelCool {
			if now.Sub(st.until) > keep {
				delete(e.modelCool, m)
			}
		}
	}
}

// SetPriority 设置账号优先级（权重乘子）。<=0 或 NaN 视为未设置（恢复 1.0 不加权）。
//
// 用途：用户手工标记"一次性登录"账号，让其额度优先被消耗。
// 乘子而非绝对优先：保持加权随机，账号在冷却/在途满时自动让位给其他账号，
// 避免被集中打爆而更快触发模型级限流（那样反而更慢用完）。
// 优先级持久化在 state.json，重启不丢。
func (p *Pool) SetPriority(uid string, priority float64) {
	if math.IsNaN(priority) || math.IsInf(priority, 0) {
		priority = 0
	}
	if priority < 0 {
		priority = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.priority = priority
		p.dirty.Store(true)
	}
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.SetCreditsReason(uid, credits, "自动刷新")
}

// SetCreditsReason 与 SetCredits 相同，但显式标注变动来源（供积分历史归因）。
func (p *Pool) SetCreditsReason(uid string, credits int64, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.noteCreditsLocked(e, uid, credits, reason)
		e.credits = credits
		p.dirty.Store(true)
	}
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）。
// 冷却入口同时是熔断器的失败信号：喂入 fails，达到阈值按指数退避熔断（与 until 正交）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		p.recordBreakerFailureLocked(e) // 冷却入口也是熔断器的失败信号
		p.dirty.Store(true)
	}
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownSoftOnly 只设置冷却、【不喂熔断计数】。
//
// 与 Cooldown 的区别（重要）：Cooldown 的语义是"冷却入口同时是熔断器的失败信号"，
// 适用于账号本身出故障的场景（429/404/5xx）。但有些场景里账号是健康的、
// 只是【暂时不可用】——典型是 11140 内容安全拒绝（上游把账号临时标记，
// 换其他账号立刻成功）。此时喂熔断会把好账号熔断掉（指数退避最长数小时），
// 误伤过重；只设一段固定冷却即可（到期自动恢复）。
//
// 不变量：本方法不改变 fails/retryCount/breakerUntil（熔断器状态完全不受影响）。
func (p *Pool) CooldownSoftOnly(uid string, d time.Duration, reason string) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	// 已有更长的硬冷却（余额耗尽到次日 4 点）时，不用较短的软冷却把它缩短。
	if e.coolKind == CoolHard && time.Now().Before(e.until) && e.until.After(time.Now().Add(d)) {
		return
	}
	e.until = time.Now().Add(d)
	e.coolKind = CoolSoft
	e.reason = reason
	p.dirty.Store(true)
}

// CooldownUntilTomorrow4AM 冷却到次日 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 所属日期的次日 04:00（与 now 同一时区）。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		p.dirty.Store(true)
	}
}

// reviveCoolingLocked 只清冷却（until/coolKind/reason）并更新 credits，不动熔断器
// （fails/retryCount/breakerUntil）。签到解冻走这里：签到成功只证明余额恢复与
// billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx 信号）不应被签到覆盖。
// 调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, uid string, credits int64, reason string) {
	p.noteCreditsLocked(e, uid, credits, reason)
	e.credits = credits
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号非禁用时，清冷却（余额恢复）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.ReenableIfCreditsReason(uid, remain, "签到")
}

// ReenableIfCreditsReason 与 ReenableIfCredits 相同，但显式标注变动来源。
func (p *Pool) ReenableIfCreditsReason(uid string, remain int64, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, uid, remain, reason)
		} else {
			p.noteCreditsLocked(e, uid, remain, reason)
			e.credits = remain
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal。
// 达到 breakerThreshold 触发熔断（指数退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		// 该账号的全部模型级冷却立即清除：一次成功请求是"上游已恢复"的最强信号，
		// 半开探测正是靠它自愈（探测请求成功会走到 NoteSuccess）。
		// 若只清计数不清 until，冷却中的账号成功后仍要干等到截止——
		// 20:22 事故里 138 账号被多晾 10 分钟就是这个原因。
		for _, st := range e.modelCool {
			st.until = time.Time{}
			st.backoffN = 0
			st.probeFails = 0
		}
		p.dirty.Store(true)
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

// AvailableUIDs 返回当前 healthy 且未占满在途名额的账号 UID 列表（按 UID 排序，稳定输出）。
// 供会话粘性路由（internal/session）做快路径命中校验 + 双段分配；无可用返回空切片。
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUID 若 uid 当前 healthy 且未占满在途名额，返回其凭证（记录 lastUsed 防撞号）；
// 否则返回 nil。供会话粘性路由命中校验与直取使用。
// PickByUID 粘性会话按 uid 直取。
// model 非空时，若该账号对【此模型】正处于 6004 冷却，则返回 nil（调用方解绑粘性
// 并回落普通轮换）——粘住一个对该模型已限流的账号只会反复撞墙。
func (p *Pool) PickByUID(uid, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if model != "" && e.modelCooling(model, now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告池当前是否可服务：存在至少一个 healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
func (p *Pool) ServableNow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if e.healthy(now) && !p.inFlightFull(e) {
			return true
		}
	}
	return false
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
	st := Status{
		UID:             uid,
		Nickname:        e.a.Nickname,
		Credits:         e.credits,
		CreditsKnown:    e.creditsKnown,
		Cooling:         now.Before(e.until) || now.Before(e.breakerUntil),
		Reason:          e.reason,
		Disabled:        e.disabled,
		SuccessCount:    e.successCount,
		ErrTotal:        e.errTotal,
		LastSuccessTime: e.lastSuccess,
		LastErrTime:     e.lastErr,
		Until:           e.until,
		InFlight:        int(e.inFlight.Load()),
		BreakerFails:    e.fails,
		Priority:        e.priority,
		BreakerUntil:    e.breakerUntil,
	}
	if pk, ok := e.peakVisible(now); ok {
		st.InFlightPeak = pk
		st.PeakActive = true
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。
		st.CoolRemaining = int64(time.Until(e.until).Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
	}
	return st
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
	p.applyAccountsLocked(sf.Accounts)
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		p.byUID[uid] = &entry{
			a:            &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:      s.Credits,
			creditsKnown: s.CreditsKnown,
			disabled:     s.Disabled,
			reason:       s.Reason,
			until:        s.Until,
			coolKind:     s.CoolKind,
			successCount: s.SuccessCount,
			errTotal:     errTotal,
			lastErr:      s.LastErr,
			lastSuccess:  s.LastSuccess,
			// 优先级从 state 恢复：重启后用户的手工标记不丢（无法二次登录的账号
			// 重新标记成本很高，必须持久化）。
			priority: s.Priority,
		}
	}
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := p.stateOverviewLocked()
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.notePersistFail(err)
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, p.stateFp); err != nil {
		p.notePersistFail(err)
		return
	}
	if p.persistFails > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("pool: state.json 落盘恢复（此前连续失败 %d 次）", p.persistFails)
		p.persistFails = 0
	}

	// 同步镜像一份快照到 Redis（fire-and-forget），与本地 state.json 并存作恢复备份。
	if p.store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			p.store.SaveState(snapRaw)
		}
	}
}

// notePersistFail 记录一次本地 state.json 落盘失败，并按节流规则决定是否打日志：
// 首败（状态成功→失败）打完整错误、每 persistLogEvery 次连续失败打一条提醒、
// 其余连续失败静默（flusher 5s 一把，磁盘持续满时不刷屏）。
// 恢复成功的日志由 saveLocked 在成功路径统一打。与 redisstore 三处异步写的
// "失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是盲区，故多一层节流（notification）。
func (p *Pool) notePersistFail(err error) {
	if p.persistFails == 0 {
		log.Printf("pool: state.json 落盘失败: %v", err)
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("pool: state.json 连续落盘失败 %d 次: %v", p.persistFails, err)
	}
	p.persistFails++
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。调用方必须已持 p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = stateAccount{
			Credits:      e.credits,
			CreditsKnown: e.creditsKnown,
			Disabled:     e.disabled,
			Reason:       e.reason,
			Until:        e.until,
			CoolKind:     e.coolKind,
			SuccessCount: e.successCount,
			ErrTotal:     e.errTotal,
			LastSuccess:  e.lastSuccess,
			LastErr:      e.lastErr,
			Priority:     e.priority,
		}
	}
	return sf
}
