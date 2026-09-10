package scheduler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newCreditTestScheduler 构建一个指向 fake 上游的调度器。
func newCreditTestScheduler(t *testing.T, f *fakeUpstream, srv *httptest.Server, p *pool.Pool, interval time.Duration) *Scheduler {
	t.Helper()
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	return New(Config{
		Pool:                  p,
		Upstream:              up,
		CheckinHours:          []int{9, 21},
		KeepaliveHours:        []int{22},
		CreditRefreshInterval: interval,
	})
}

// TestRunCreditRefreshNow 验证手动/定时刷新会把上游余额写回池（供 GUI 显示）。
func TestRunCreditRefreshNow(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 1234}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	var gotUID string
	var gotRemain int64
	s := newCreditTestScheduler(t, f, srv, p, 0)
	s.cfg.OnCreditRefresh = func(uid string, remain int64, err error) {
		gotUID, gotRemain = uid, remain
	}

	n := s.RunCreditRefreshNow()
	if n != 1 {
		t.Errorf("refreshed=%d want 1", n)
	}
	st, _ := p.Status("u1")
	if st.Credits != 1234 {
		t.Errorf("credits=%d want 1234", st.Credits)
	}
	if gotUID != "u1" || gotRemain != 1234 {
		t.Errorf("callback uid=%q remain=%d", gotUID, gotRemain)
	}
}

// TestRunCreditRefreshSkipsDisabled 禁用账号不参与刷新（避免无谓上游请求）。
func TestRunCreditRefreshSkipsDisabled(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")

	s := newCreditTestScheduler(t, f, srv, p, 0)
	if n := s.RunCreditRefreshNow(); n != 0 {
		t.Errorf("禁用账号不应被刷新，got %d", n)
	}
}

// TestCreditRefreshLoopDisabledWhenZeroInterval 间隔为 0 时不启动刷新循环。
// 这是"关闭定时刷新"配置项的语义保证。
func TestCreditRefreshLoopDisabledWhenZeroInterval(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	// interval=0 → Run 不应启动 creditRefreshLoop
	s := newCreditTestScheduler(t, f, srv, p, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()

	st, _ := p.Status("u1")
	if st.Credits != 0 {
		t.Errorf("间隔为 0 时不应自动刷新，credits=%d", st.Credits)
	}
}

// TestCreditRefreshLoopRunsAndStopsCleanly 正间隔时循环会执行，且 ctx 取消后干净退出。
func TestCreditRefreshLoopRunsAndStopsCleanly(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 777}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s := newCreditTestScheduler(t, f, srv, p, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	// 轮询等待首次刷新生效（避免使用固定 sleep 造成 flake）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := p.Status("u1"); st.Credits == 777 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	if st, _ := p.Status("u1"); st.Credits != 777 {
		t.Errorf("定时刷新未生效，credits=%d want 777", st.Credits)
	}
}
