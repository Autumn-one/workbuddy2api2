// Package scheduler 定时任务：每日签到（09/21点）+ token keepalive（22点）。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

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
func (s *Scheduler) RunCreditRefreshNow() int {
	ok := 0
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
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

// checkinReason 把签到结果映射为积分变动归因文案（供积分历史展示）。
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
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		// 签到：成功 / 今天已签到 / 失败 三态；后两者都不断后续余额查询
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
			log.Printf("checkin %s: ok", st.UID)
		case CheckinAlready:
			log.Printf("checkin %s: 今天已签到", st.UID)
		default:
			log.Printf("checkin %s: %s", st.UID, detail)
		}
		if s.cfg.OnCheckin != nil {
			s.cfg.OnCheckin(st.UID, status, detail)
		}
		remain, err := s.cfg.Upstream.UserResource(a)
		if err != nil {
			log.Printf("user-resource %s: %v", st.UID, err)
			continue
		}
		s.cfg.Pool.ReenableIfCreditsReason(st.UID, remain, checkinReason(status))
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
