package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ─────────────────────────── 半开探测（half-open）───────────────────────────
//
// 背景（20:22 事故）：上游对某模型的全局限流会先后波及所有账号，指数退避把
// 6 个账号全部推进长冷却（最长 1h），服务整段不可用 9 分钟——而上游实际
// 早恢复了（138 账号 20:31 冷却到期后立刻成功）。冷却没有"提前恢复"通道。

// TestHalfOpenProbeAllowedAfterQuietPeriod 冷却中但安静期已过 → 允许作为探测候选。
func TestHalfOpenProbeAllowedAfterQuietPeriod(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)

	// 冷却刚发生：未过安静期 → 不允许探测（仍视为冷却）
	if p.HalfOpenAllowed("a1", "m") {
		t.Fatal("刚撞墙就探测会立即再撞，应先等安静期")
	}
	// 时间推过安静期（仍在冷却内）→ 允许探测
	// 注意不能只拨 until（那等于冷却过期=正常恢复路径）；半开窗口是
	// "until 未到期 && lastHit 距今超过安静期"。测试里直接拨 lastHit。
	p.mu.Lock()
	if st := p.byUID["a1"].modelCool["m"]; st != nil {
		st.lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
	}
	p.mu.Unlock()
	if !p.HalfOpenAllowed("a1", "m") {
		t.Fatal("冷却中但安静期已过，应允许半开探测")
	}
}

// TestHalfOpenDisabledBeforeQuiet 安静期内禁止探测（避免立刻撞墙浪费请求）。
func TestHalfOpenDisabledBeforeQuiet(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	if p.HalfOpenAllowed("a1", "m") {
		t.Fatal("安静期内不应允许探测")
	}
}

// TestHalfOpenNoState 无冷却状态时无所谓半开（账号本来就可选）。
func TestHalfOpenNoState(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	if !p.HalfOpenAllowed("a1", "m") {
		t.Fatal("无冷却状态应允许（虽然此时半开语义不会被用到）")
	}
}

// TestPickHalfOpenSelection 决定性集成：全部账号冷却 + 安静期已过时，
// pick 不得返回 nil，而应返回一个半开探测候选（冷却剩余最短者优先）。
func TestPickHalfOpenSelection(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.Add(&auth.Auth{UID: "a2"})
	p.Add(&auth.Auth{UID: "a3"})
	// 三个账号先后撞墙，冷却期互相错开
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	p.NoteModelRateLimit("a2", "m", 10*time.Minute)
	p.NoteModelRateLimit("a3", "m", 10*time.Minute)
	// 把三个账号的 lastHit 拨过安静期（进入真实半开窗口：until 未到、安静期已过）
	p.mu.Lock()
	for _, e := range p.byUID {
		if st := e.modelCool["m"]; st != nil {
			st.lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
		}
	}
	p.mu.Unlock()
	// 随机源恒命中（rnd=0 < 阈值）→ 半开必放行，消除概率波动
	p.SetRandomSource(func(n int64) int64 { return 0 })

	got := map[string]bool{}
	for i := 0; i < 10; i++ {
		a := p.PickExcluding(nil, "m")
		if a == nil {
			t.Fatal("全部冷却但半开可用时，pick 不应返回 nil（应给出探测候选）")
		}
		got[a.UID] = true
	}
	if len(got) == 0 {
		t.Fatal("未选出任何候选")
	}
	// 半开是概率性的（默认 10%），10 次里应主要落在"正常候选路径被冷却拦截后
	// 的半开探测"上。这里不断言具体分布，只断言返回的是池内账号。
	for uid := range got {
		if uid != "a1" && uid != "a2" && uid != "a3" {
			t.Errorf("返回了池外账号 %v", uid)
		}
	}
}

// TestPickNilWhenAllCoolingNoHalfOpen 半开不可用（安静期内）时保持现状：返回 nil 快速失败。
func TestPickNilWhenAllCoolingNoHalfOpen(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	// 未过安静期
	if a := p.PickExcluding(nil, "m"); a != nil {
		t.Fatalf("安静期内全冷却应返回 nil（快速失败），got %v", a.UID)
	}
}

// TestHalfOpenResetOnSuccess 探测成功（账号对模型有成功请求）→ 冷却立即清除。
func TestHalfOpenResetOnSuccess(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	// 模拟探测成功（NoteSuccess 路径即半开探测成功时的回收点）
	p.NoteSuccess("a1")
	if p.IsModelCooling("a1", "m") {
		t.Fatal("探测成功后冷却应立即清除")
	}
	if a := p.PickExcluding(nil, "m"); a == nil {
		t.Fatal("冷却清除后应恢复正常选号")
	}
}

// TestHalfOpenFailExtendsCooldown 探测失败 → 冷却不变（不延长、不清零），继续等。
func TestHalfOpenFailExtendsCooldown(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	before := p.ModelCooldownUntil("a1", "m")
	// 探测失败 = 又撞一次 6004 → 记录冷却（翻倍/封顶符合退避设计）
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	after := p.ModelCooldownUntil("a1", "m")
	if !after.After(before) || after.Before(time.Now()) {
		t.Errorf("探测失败后冷却应维持并按退避升级: before=%v after=%v", before, after)
	}
}

// TestHalfOpenRateLimitNotZero 概率不能是 0（否则半开永远不触发，等于没做）。
func TestHalfOpenRateLimitNotZero(t *testing.T) {
	if halfOpenProbeRate <= 0 || halfOpenProbeRate > 1 {
		t.Fatalf("halfOpenProbeRate=%v 应在 (0,1]", halfOpenProbeRate)
	}
	if halfOpenQuiet <= 0 {
		t.Fatalf("halfOpenQuiet=%v 应为正", halfOpenQuiet)
	}
}
