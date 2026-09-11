package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ───────────────── 账号优先级（手工标记 → 权重乘子）─────────────────
//
// 需求：用户知道哪些账号是"一次性登录"的（手机号一次性，失效后无法二次登录），
// 希望这类账号优先被消耗掉。做法就是给账号设优先级，权重乘上去。
//
// 设计约束：
//  1. 优先级是【权重乘子】而非绝对优先——保持加权随机，让它在冷却/在途满时
//     自动让位给其他账号（集中打一个账号会更快撞 6004，反而更慢用完）。
//  2. 优先级必须【持久化】：重启后标记不能丢（用户无法二次登录，重新标记成本高）。
//  3. 默认 1.0 且不加权：未标记账号行为与现在完全一致（零行为变更）。

// TestPriorityDefaultIsNeutral 未设置优先级 → 乘子 1.0，权重与现状一致。
func TestPriorityDefaultIsNeutral(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.SetCredits("a1", 100)

	p.mu.RLock()
	e := p.byUID["a1"]
	got := p.weightOf(e, 100, time.Now())
	p.mu.RUnlock()

	// 手工按现有公式复算：1 + 100/100*10 + idleWeightMax(从未使用) + 1.5(无记录)
	want := 1.0 + 10.0 + p.idleWeightMax + 1.5
	if got != want {
		t.Errorf("默认权重=%.2f want %.2f（未标记账号行为不得变化）", got, want)
	}
}

// TestPriorityScalesWeight 优先级直接乘在总分上。
func TestPriorityScalesWeight(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.Add(&auth.Auth{UID: "a2"})
	p.SetCredits("a1", 100)
	p.SetCredits("a2", 100)

	p.mu.RLock()
	base := p.weightOf(p.byUID["a1"], 100, time.Now())
	p.mu.RUnlock()

	p.SetPriority("a1", 3.0)
	p.mu.RLock()
	got := p.weightOf(p.byUID["a1"], 100, time.Now())
	plain := p.weightOf(p.byUID["a2"], 100, time.Now())
	p.mu.RUnlock()

	if got != base*3.0 {
		t.Errorf("优先级 3.0 后权重=%.2f want %.2f", got, base*3.0)
	}
	if plain != base {
		t.Errorf("未设置优先级的账号权重被影响: %.2f want %.2f", plain, base)
	}
}

// TestPriorityAffectsPick 集成：高优先级账号在同等条件下应明显更常被选中。
func TestPriorityAffectsPick(t *testing.T) {
	withNoPickGap(t)
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.Add(&auth.Auth{UID: "a2"})
	p.Add(&auth.Auth{UID: "a3"})
	// 三者积分相同 → 权重差异只来自优先级
	p.SetCredits("a1", 1000)
	p.SetCredits("a2", 1000)
	p.SetCredits("a3", 1000)
	p.SetPriority("a1", 5.0) // a1 优先消耗

	counts := map[string]int{}
	for i := 0; i < 3000; i++ {
		counts[p.Pick().UID]++
	}
	if counts["a1"] <= counts["a2"] || counts["a1"] <= counts["a3"] {
		t.Fatalf("高优先级账号应被更常选中: %v", counts)
	}
	// 但不得垄断：其他账号仍需被选中（加权随机而非绝对优先）
	if counts["a2"] == 0 || counts["a3"] == 0 {
		t.Fatalf("高优先级不得垄断选号（否则会更快撞限流）: %v", counts)
	}
	t.Logf("选中分布: %v", counts)
}

// TestPriorityPersistsAcrossRestart 优先级必须落盘：重启后标记不丢。
func TestPriorityPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"

	p := New(fp)
	p.Add(&auth.Auth{UID: "a1"})
	p.Add(&auth.Auth{UID: "a2"})
	p.SetPriority("a1", 3.5)
	p.Flush()

	// 模拟重启
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "a1"})
	p2.Add(&auth.Auth{UID: "a2"})

	p2.mu.RLock()
	got := p2.byUID["a1"].priority
	other := p2.byUID["a2"].priority
	p2.mu.RUnlock()

	if got != 3.5 {
		t.Errorf("重启后 a1 优先级=%.2f want 3.5", got)
	}
	if other != 0 {
		t.Errorf("未标记账号优先级应为 0（=未设置）, got %.2f", other)
	}
}

// TestPriorityInvalidValuesIgnored 非法值防御：0/负/NaN 一律视为未设置（1.0）。
func TestPriorityInvalidValuesIgnored(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.SetCredits("a1", 100)

	for _, v := range []float64{0, -1, -3.5} {
		p.SetPriority("a1", v)
		p.mu.RLock()
		w := p.weightOf(p.byUID["a1"], 100, time.Now())
		p.mu.RUnlock()
		// 应等于未设置时的权重
		want := 1.0 + 10.0 + p.idleWeightMax + 1.5
		if w != want {
			t.Errorf("优先级 %v 应被忽略（视为 1.0）: 权重=%.2f want %.2f", v, w, want)
		}
	}
}

// TestPriorityUnknownUIDSafe 对不存在的账号设置优先级不 panic。
func TestPriorityUnknownUIDSafe(t *testing.T) {
	p := newModelTestPool(t)
	p.SetPriority("nobody", 5.0) // 不应 panic
	p.mu.RLock()
	_, ok := p.byUID["nobody"]
	p.mu.RUnlock()
	if ok {
		t.Error("不应为不存在的账号创建 entry")
	}
}

// TestStatusExposesPriority GUI/status 需要看到优先级（否则无法确认标记是否生效）。
func TestStatusExposesPriority(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1", Nickname: "一次性"})
	p.SetPriority("a1", 3.0)
	st, ok := p.Status("a1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Priority != 3.0 {
		t.Errorf("Status.Priority=%.2f want 3.0（GUI 需要它来显示标记）", st.Priority)
	}
}
