package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ───────────────── 半开探测的自愈节流修复 ─────────────────
//
// 缺陷（02:28~02:34 生产日志 + 复现实验证实）：
// 半开放行的探测请求失败后，NoteModelRateLimit 刷新 lastHit → 安静期重新计时。
// 探测节奏变成「每 2 分钟至多 1 次，且还要过 10% 概率门」——10% × 每 2 分钟一次
// = 期望 0.5 次/10 分钟。生产 02:28:44~02:34 的 13 次请求 0 次放行，服务断供
// 整整 6 分钟（138 账号实际早已恢复）。
//
// 修复语义：
//   - 概率门只用于【首次】探测（N 个账号同时半开时防惊群）；
//   - 一旦放行并失败，该（账号×模型）后续每过安静期【必然】放行一次探测
//     （保底自愈节奏：每 2 分钟至多 1 次探测请求，10 分钟窗口保底 5 次）；
//   - 探测成功 → NoteSuccess 清冷却，回到正常选号。

// TestGuaranteedProbeAfterFirstFailure 首次探测失败后，过安静期必放行（概率门豁免）。
//
// 流程：首次探测由概率门决定（恒不命中注入 → 被挡）→ 手动模拟"放行后探测失败"
// （pickHalfOpenLocked 打标 probeFails，生产里放行即真实请求）→ 再过安静期 → 必放行。
func TestGuaranteedProbeAfterFirstFailure(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	p.SetRandomSource(func(n int64) int64 { return n }) // 概率门恒不命中

	// 安静期内：不放行
	if a := p.PickExcluding(nil, "m"); a != nil {
		t.Fatal("安静期内不应放行")
	}
	// 进入半开窗口：概率门挡住首次探测（恒不命中注入）
	if a := p.PickExcluding(nil, "m"); a != nil {
		t.Fatalf("概率门应挡住首次探测, got %v", a.UID)
	}
	// 模拟"首次探测已放行且失败"：probeFails++（与 pickHalfOpenLocked 放行时的打标一致）
	p.mu.Lock()
	p.byUID["a1"].modelCool["m"].probeFails++
	p.byUID["a1"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
	p.mu.Unlock()

	// 修复后：已探测失败过 → 必放行（不再过概率门）
	a := p.PickExcluding(nil, "m")
	if a == nil {
		t.Fatal("已探测失败过的账号过安静期后必放行探测（保底自愈节奏）")
	}
	if a.UID != "a1" {
		t.Fatalf("应选 a1, got %v", a.UID)
	}
}

// TestGuaranteedProbeCadence 修复后的节流：每过安静期恰放行一次，
// 连续 5 个窗口 = 5 次探测（10 分钟窗口保底 5 次）。
func TestGuaranteedProbeCadence(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	p.SetRandomSource(func(n int64) int64 { return n }) // 概率门恒不命中

	// 首次探测失败（打标 probeFails，等价于半开放行后真实请求失败）
	p.mu.Lock()
	p.byUID["a1"].modelCool["m"].probeFails = 1
	p.mu.Unlock()

	released := 0
	for round := 0; round < 5; round++ {
		// 模拟时间流逝一个安静期
		p.mu.Lock()
		if st := p.byUID["a1"].modelCool["m"]; st != nil {
			st.lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
		}
		p.mu.Unlock()
		if a := p.PickExcluding(nil, "m"); a != nil {
			released++
			// 每次放行的探测都失败（生产就是这样）→ probeFails 保持 >0
		}
	}
	if released != 5 {
		t.Fatalf("5 个安静期窗口应放行 5 次探测（保底自愈），实际 %d", released)
	}
}

// TestFirstProbeStillProbabilistic 首次探测仍走概率门（防惊群语义保留）。
func TestFirstProbeStillProbabilistic(t *testing.T) {
	p := newModelTestPool(t)
	for _, uid := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"} {
		p.Add(&auth.Auth{UID: uid})
		p.NoteModelRateLimit(uid, "m", 10*time.Minute)
		// 全部进入半开窗口
		p.mu.Lock()
		if st := p.byUID[uid].modelCool["m"]; st != nil {
			st.lastHit = time.Now().Add(-(halfOpenQuiet + time.Minute))
		}
		p.mu.Unlock()
	}
	// 随机源恒不命中 → 10 个账号无一放行（防惊群：不是全放）
	p.SetRandomSource(func(n int64) int64 { return n })
	for range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"} {
		if a := p.PickExcluding(nil, "m"); a != nil {
			t.Fatalf("概率门恒不命中时不应放行（防惊群）, got %v", a.UID)
		}
	}
}

// TestProbeSuccessStillHeals 修复不破坏自愈闭环：放行的探测成功 → 冷却清除。
func TestProbeSuccessStillHeals(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	p.NoteModelRateLimit("a1", "m", 10*time.Minute) // 首次探测失败
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.mu.Lock()
	p.byUID["a1"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
	p.mu.Unlock()

	a := p.PickExcluding(nil, "m")
	if a == nil {
		t.Fatal("应放行探测")
	}
	p.NoteSuccess(a.UID) // 探测请求成功
	if p.IsModelCooling("a1", "m") {
		t.Fatal("探测成功后冷却应清除")
	}
}
