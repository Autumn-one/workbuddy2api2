// Package scheduler 定时任务：每日签到（09/21点）+ token keepalive（22点）。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// CheckinStatus 签到结果分类。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 今天已签到（正常，非故障）
	CheckinFailed  CheckinStatus = "failed"  // 其它失败
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	KeepaliveHours []int // 默认 [22]

	// CreditRefreshInterval 额度刷新周期；<=0 关闭定时刷新。
	// 串行 + 间隔，避免触发上游 billing 限流。
	CreditRefreshInterval time.Duration

	// OnCheckin 每个账号签到后回调，供 GUI 展示"签到记录"；nil 时静默。
	OnCheckin func(uid string, status CheckinStatus, detail string)
	// OnKeepalive 每个账号 token 刷新后回调；nil 时静默。
	OnKeepalive func(uid string, ok bool, detail string)
	// OnCreditRefresh 额度刷新后回调（uid, remain, err）；nil 时静默。
	OnCreditRefresh func(uid string, remain int64, err error)
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
	// mu 保护 lastCheckinFailed（checkinOne 可能并发调用）。
	mu sync.Mutex
	// lastCheckinFailed 最近一次 RunCheckinNow 中失败的账号（供 retryFailedCheckins）。
	lastCheckinFailed []string
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	return &Scheduler{cfg: cfg}
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

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	// 额度刷新独立 ticker（周期型，而非整点型）：与签到的整点调度解耦。
	if s.cfg.CreditRefreshInterval > 0 {
		go s.creditRefreshLoop(ctx)
	}
	all := append(append([]int{}, s.cfg.CheckinHours...), s.cfg.KeepaliveHours...)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.CheckinHours, h) {
				s.RunCheckinNow()
				s.retryFailedCheckins(ctx)
			}
			if contains(s.cfg.KeepaliveHours, h) {
				s.RunKeepaliveNow()
			}
		}
	}
}

// creditRefreshLoop 周期性刷新所有账号额度显示值。
//
// 设计约束（防限流，与项目既有 credit 工具同口径）：
//   - 串行执行，每账号之间 200ms 间隔，绝不并发打上游 billing；
//   - 单个账号失败只跳过该账号，不影响其余；
//   - 只更新显示用积分（SetCredits），不改变任何冷却/熔断状态。
func (s *Scheduler) creditRefreshLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.CreditRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunCreditRefreshNow()
		}
	}
}

// RunCreditRefreshNow 立即刷新所有账号额度（供定时循环与 GUI 手动按钮共用）。
// 禁用账号跳过；返回成功刷新的账号数。
//
// 并发策略（代理感知）：代理开启时每账号走【独立出口 IP】，上游看到的限流维度
// 是单 IP，互不干扰——此时并发（worker=8）安全且把 30s+ 压到几秒；
// 直连时所有请求同一 IP，保持【串行 + 200ms 间隔】防触发上游按 IP 限流。
func (s *Scheduler) RunCreditRefreshNow() int {
	tasks := make([]pool.Status, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		tasks = append(tasks, st)
	}
	if s.cfg.Upstream != nil && s.cfg.Upstream.ProxySelectorEnabled() {
		return s.runCreditRefreshConcurrent(tasks)
	}
	return s.runCreditRefreshSerial(tasks)
}

// runCreditRefreshSerial 直连口径：串行 + 200ms 间隔（防限流）。
func (s *Scheduler) runCreditRefreshSerial(tasks []pool.Status) int {
	ok := 0
	for _, st := range tasks {
		a := s.cfg.Pool.AuthByUID(st.UID)
		remain, err := s.cfg.Upstream.UserResource(a)
		if err != nil {
			if s.cfg.OnCreditRefresh != nil {
				s.cfg.OnCreditRefresh(st.UID, 0, err)
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s.cfg.Pool.SetCreditsReason(st.UID, remain, "自动刷新")
		ok++
		if s.cfg.OnCreditRefresh != nil {
			s.cfg.OnCreditRefresh(st.UID, remain, nil)
		}
		time.Sleep(200 * time.Millisecond) // 防限流：串行 + 间隔
	}
	return ok
}

// runCreditRefreshConcurrent 代理口径：每账号独立 IP，并发刷新（worker 上限 8）。
// SetCreditsReason 内部自带锁，并发写池安全；OnCreditRefresh 回调由 GUI 侧自行投递。
func (s *Scheduler) runCreditRefreshConcurrent(tasks []pool.Status) int {
	const workers = 8
	var mu sync.Mutex
	ok := 0
	ch := make(chan pool.Status)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for st := range ch {
				a := s.cfg.Pool.AuthByUID(st.UID)
				if a == nil {
					continue
				}
				remain, err := s.cfg.Upstream.UserResource(a)
				if err != nil {
					if s.cfg.OnCreditRefresh != nil {
						s.cfg.OnCreditRefresh(st.UID, 0, err)
					}
					continue
				}
				s.cfg.Pool.SetCreditsReason(st.UID, remain, "自动刷新")
				mu.Lock()
				ok++
				mu.Unlock()
				if s.cfg.OnCreditRefresh != nil {
					s.cfg.OnCreditRefresh(st.UID, remain, nil)
				}
			}
		}()
	}
	for _, st := range tasks {
		ch <- st
	}
	close(ch)
	wg.Wait()
	return ok
}

// retryConfig 补签节奏（可注入，测试用毫秒级间隔）。
type retryConfig struct {
	interval time.Duration
	maxRound int
}

// retryFailedCheckins 定时签到后对失败账号的延迟补签。
//
// 背景：定时签到（9/21 点）若某账号失败，旧行为是干等下一个整点（12 小时），
// 期间该账号没签到、可能因余额不足无法服务。现在：签到结束后对【失败】的账号
// 隔 checkinRetryInterval 再补一轮，最多 checkinMaxRetries 轮。
//
// 只补失败账号：成功/已签到的不动（不重复打上游）；补签仍走 checkinOneQuiet
// （成功才查余额解冻，失败不多查）。goroutine 随 ctx 结束，不泄漏。
func (s *Scheduler) retryFailedCheckins(ctx context.Context) {
	failed := s.failedCheckinUIDs()
	if len(failed) == 0 {
		return
	}
	log.Printf("签到失败 %d 个账号，将于 %v 后补签（最多 %d 轮）", len(failed), checkinRetryInterval, checkinMaxRetries)
	s.retryFailedCheckinsWith(ctx, retryConfig{interval: checkinRetryInterval, maxRound: checkinMaxRetries})
}

// retryFailedCheckinsWith 与 retryFailedCheckins 同逻辑，但间隔/轮数可注入（测试用）。
// 注意：失败列表只在启动时取一次快照，之后由本循环自己跟踪——不能每轮重读
// lastCheckinFailed（RunCheckinNow 每轮会清空它，补签 goroutine 会拿到空切片）。
func (s *Scheduler) retryFailedCheckinsWith(ctx context.Context, rc retryConfig) {
	failed := s.failedCheckinUIDs()
	if len(failed) == 0 {
		return
	}
	go func() {
		todo := failed // 本循环自己的失败列表，不再读共享快照
		for round := 1; round <= rc.maxRound && len(todo) > 0; round++ {
			t := time.NewTimer(rc.interval)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			var stillFailed []string
			for _, uid := range todo {
				a := s.cfg.Pool.AuthByUID(uid)
				if a == nil || a.RefreshToken == "" {
					continue
				}
				if s.checkinOneQuiet(uid, a) {
					continue // 成功/已签到
				}
				stillFailed = append(stillFailed, uid)
				time.Sleep(200 * time.Millisecond) // 补签也节流（数量少，串行即可）
			}
			todo = stillFailed
			if len(todo) > 0 && round < rc.maxRound {
				log.Printf("补签第 %d 轮后仍失败 %d 个，%v 后再试", round, len(todo), rc.interval)
			}
		}
		if len(todo) > 0 {
			log.Printf("补签 %d 轮后仍失败 %d 个账号，等下一个整点定时签到", rc.maxRound, len(todo))
		}
	}()
}

// checkinRetryInterval 签到失败后的补签间隔。
// 30 分钟：足够长（上游临时故障/限流有恢复窗口），又不至于让当天积分断档太久。
const checkinRetryInterval = 30 * time.Minute

// checkinMaxRetries 单次定时签到后的最大补签轮数（防失败账号被无限重试打成风暴）。
const checkinMaxRetries = 2

// failedCheckinUIDs 返回最近一次 RunCheckinNow 中失败的账号 UID（由 checkinOne 记录）。
// 用最后一次签到结果快照判定，避免重新打上游。
func (s *Scheduler) failedCheckinUIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.lastCheckinFailed))
	out = append(out, s.lastCheckinFailed...)
	return out
}

// checkinOneQuiet 单账号补签；返回 true = 成功或已签到（不再需要补）。
// 与 checkinOne 的差别：不计入 lastCheckinFailed（那是定时签到当次的快照），
// 日志/回调照常（用户能在签到记录里看到补签痕迹）。
func (s *Scheduler) checkinOneQuiet(uid string, a *auth.Auth) bool {
	status, detail := CheckinOK, ""
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		detail = err.Error()
		if upstream.IsAlreadyCheckedIn(detail) {
			status = CheckinAlready
		} else {
			status = CheckinFailed
		}
	}
	log.Printf("checkin-retry %s: %s", uid, checkinLogText(status, detail))
	if s.cfg.OnCheckin != nil {
		s.cfg.OnCheckin(uid, status, detail)
	}
	if status == CheckinFailed {
		return false
	}
	if remain, err := s.cfg.Upstream.UserResource(a); err == nil {
		s.cfg.Pool.ReenableIfCreditsReason(uid, remain, "签到（失败补签）")
	}
	return true
}

// checkinLogText 签到结果的日志文案。
func checkinLogText(status CheckinStatus, detail string) string {
	switch status {
	case CheckinOK:
		return "ok"
	case CheckinAlready:
		return "今天已签到"
	default:
		return "失败: " + detail
	}
}
func checkinReason(status CheckinStatus) string {
	switch status {
	case CheckinOK:
		return "签到"
	case CheckinAlready:
		return "签到（今天已签到）"
	default:
		return "签到失败后余额查询"
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
//
// 签到失败时【不再】紧跟余额查询：失败已证明该账号此刻不可用，
// 再查一次余额只会多一次上游往返（原实现失败也查，纯属浪费）。
// 并发策略同 RunCreditRefreshNow：代理开时按独立 IP 并发，直连时串行节流。
func (s *Scheduler) RunCheckinNow() {
	// 清空上次失败快照：本轮重新累计（补签用）。
	s.mu.Lock()
	s.lastCheckinFailed = nil
	s.mu.Unlock()
	tasks := make([]pool.Status, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		tasks = append(tasks, st)
	}
	one := func(st pool.Status) {
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			return
		}
		s.checkinOne(st.UID, a)
	}
	if s.cfg.Upstream != nil && s.cfg.Upstream.ProxySelectorEnabled() {
		s.runConcurrent(tasks, one)
		return
	}
	for _, st := range tasks {
		one(st)
		time.Sleep(200 * time.Millisecond) // 直连口径：串行 + 间隔防限流
	}
}

// checkinOne 单账号签到 + （成功/已签到时）余额刷新与解冻。
// 失败会记入 lastCheckinFailed（供 retryFailedCheckins 补签）。
func (s *Scheduler) checkinOne(uid string, a *auth.Auth) {
	status, detail := CheckinOK, ""
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		detail = err.Error()
		if upstream.IsAlreadyCheckedIn(detail) {
			status = CheckinAlready
		} else {
			status = CheckinFailed
		}
	}
	switch status {
	case CheckinOK:
		log.Printf("checkin %s: ok", uid)
	case CheckinAlready:
		log.Printf("checkin %s: 今天已签到", uid)
	default:
		log.Printf("checkin %s: %s", uid, detail)
	}
	if s.cfg.OnCheckin != nil {
		s.cfg.OnCheckin(uid, status, detail)
	}
	// 签到失败不再查余额：失败已证明不可用，多查一次纯属浪费上游往返。
	if status == CheckinFailed {
		s.mu.Lock()
		s.lastCheckinFailed = append(s.lastCheckinFailed, uid)
		s.mu.Unlock()
		return
	}
	remain, err := s.cfg.Upstream.UserResource(a)
	if err != nil {
		log.Printf("user-resource %s: %v", uid, err)
		return
	}
	s.cfg.Pool.ReenableIfCreditsReason(uid, remain, checkinReason(status))
}

// runConcurrent 代理口径：worker=8 并发执行 fn。直连不走这里（保持串行节流）。
func (s *Scheduler) runConcurrent(tasks []pool.Status, fn func(pool.Status)) {
	const workers = 8
	ch := make(chan pool.Status)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for st := range ch {
				fn(st)
			}
		}()
	}
	for _, st := range tasks {
		ch <- st
	}
	close(ch)
	wg.Wait()
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
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "12153 session dead")
			}
			if s.cfg.OnKeepalive != nil {
				s.cfg.OnKeepalive(st.UID, false, err.Error())
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
		if s.cfg.OnKeepalive != nil {
			s.cfg.OnKeepalive(st.UID, true, "")
		}
	}
}
