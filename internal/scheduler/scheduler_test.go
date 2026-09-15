package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// fakeUpstream 同时模拟 billing 与 refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	resourceCalls  atomic.Int32
	checkinErr     string // 非空时 daily-checkin 返回 400
	resourceRemain int64
	checkinDelay   time.Duration // 模拟慢上游，用于验证并发
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			if f.checkinDelay > 0 {
				time.Sleep(f.checkinDelay)
			}
			if f.checkinErr != "" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"code":10002,"msg":"` + f.checkinErr + `"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			f.resourceCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestCheckinFailedSkipsResourceQuery 签到失败的账号不再紧跟余额查询。
// 失败已证明该账号此刻不可用，再查一次余额只会多一次上游往返（浪费）。
func TestCheckinFailedSkipsResourceQuery(t *testing.T) {
	f := &fakeUpstream{checkinErr: "session expired"}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}, KeepaliveHours: []int{22}})

	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d want 1", f.checkinCalls.Load())
	}
	if f.resourceCalls.Load() != 0 {
		t.Errorf("签到失败后不应再查余额, resource calls=%d want 0", f.resourceCalls.Load())
	}
}

// TestCheckinConcurrentWhenProxyEnabled 代理开启时签到并发执行：
// 每账号走独立出口 IP，限流维度是单 IP，互不干扰。串行 30s+ → 并发几秒。
func TestCheckinConcurrentWhenProxyEnabled(t *testing.T) {
	const n = 6
	f := &fakeUpstream{resourceRemain: 100, checkinDelay: 120 * time.Millisecond}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%d", i), AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	up.SetProxySelector(func(uid string) string { return "http://127.0.0.1:1" }) // 仅用于开启代理标记

	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	start := time.Now()
	s.RunCheckinNow()
	elapsed := time.Since(start)

	if f.checkinCalls.Load() != n {
		t.Errorf("checkin calls=%d want %d", f.checkinCalls.Load(), n)
	}
	// 串行 + 120ms/账号 ≈ 720ms；并发 worker=8 应远小于串行耗时。
	if elapsed > time.Duration(n)*120*time.Millisecond/2 {
		t.Errorf("代理开启时应并发执行，耗时 %v 疑似串行", elapsed)
	}
}

// TestCreditRefreshConcurrentWhenProxyEnabled 代理开启时额度刷新并发执行。
func TestCreditRefreshConcurrentWhenProxyEnabled(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 777}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	for i := 0; i < 5; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%d", i), AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	up.SetProxySelector(func(uid string) string { return "http://127.0.0.1:1" })

	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	n := s.RunCreditRefreshNow()
	if n != 5 {
		t.Errorf("refreshed=%d want 5", n)
	}
	if f.resourceCalls.Load() != 5 {
		t.Errorf("resource calls=%d want 5", f.resourceCalls.Load())
	}
}

// TestRetryFailedCheckinsSucceedsOnRetry 核心：定时签到失败的账号，补签能成功。
// 第一次签到失败，30ms 后补签成功——验证"失败会补签"链路闭环。
func TestRetryFailedCheckinsSucceedsOnRetry(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 800}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}, KeepaliveHours: []int{22}})

	// 第一轮：签到失败
	f.checkinErr = "temporary error"
	s.RunCheckinNow()
	if got := s.failedCheckinUIDs(); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("失败快照=%v want [u1]", got)
	}
	// 恢复上游，补签应成功
	f.checkinErr = ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.retryFailedCheckinsWith(ctx, retryConfig{interval: 30 * time.Millisecond, maxRound: 2})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := p.Status("u1")
		if st.Credits == 800 {
			return // 补签成功且已解冻刷新额度
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := p.Status("u1")
	t.Errorf("补签后积分=%d want 800（补签未生效）", st.Credits)
}

// TestRetryFailedCheckinsStopsAfterMaxRound 补签最多 checkinMaxRetries 轮，
// 防失败账号被无限重试打成风暴。
func TestRetryFailedCheckinsStopsAfterMaxRound(t *testing.T) {
	f := &fakeUpstream{checkinErr: "permanent error"}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}, KeepaliveHours: []int{22}})

	s.RunCheckinNow() // 第 1 次失败
	callsAfterFirst := f.checkinCalls.Load()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.retryFailedCheckinsWith(ctx, retryConfig{interval: 20 * time.Millisecond, maxRound: 2})

	// 等补签轮跑完：2 轮 × (20ms 间隔 + 200ms 节流) ≈ 440ms，给足余量。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.checkinCalls.Load() >= callsAfterFirst+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // 再等确认没有第 3 轮
	total := f.checkinCalls.Load()
	// 1 次原始 + 2 轮补签 = 3 次，不应更多
	if total != callsAfterFirst+2 {
		t.Errorf("checkin 总调用=%d want %d（1 原始 + 2 补签，不应无限重试）", total, callsAfterFirst+2)
	}
}

// TestRetryFailedCheckinsNoRetryWhenAllSucceed 全部成功时不产生补签。
func TestRetryFailedCheckinsNoRetryWhenAllSucceed(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CheckinHours: []int{9}, KeepaliveHours: []int{22}})

	s.RunCheckinNow()
	if got := s.failedCheckinUIDs(); len(got) != 0 {
		t.Errorf("全成功时失败快照应为空, got %v", got)
	}
	callsBefore := f.checkinCalls.Load()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.retryFailedCheckinsWith(ctx, retryConfig{interval: 10 * time.Millisecond, maxRound: 2})
	time.Sleep(100 * time.Millisecond)
	if f.checkinCalls.Load() != callsBefore {
		t.Errorf("全成功时不应补签, checkin 调用 %d → %d", callsBefore, f.checkinCalls.Load())
	}
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("should disable session-dead account: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// 不应 panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}
