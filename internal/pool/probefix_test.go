package pool

import (
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ─────────────── 半开保底语义的确定性修复（审计缺陷 ①）───────────────
//
// 缺陷：判定「是否已有候选探测失败过」时读 cands[0]，但 cands 由 map 遍历生成
// （顺序随机）且排序发生在判定之后 → 混合场景下保底语义随机失效约 15%。
// 修复后要求：只要【存在任一】已探测过的候选，就必放行（与候选顺序无关）。

// TestGuaranteedProbeDeterministicAcrossOrder 重复多次（map 顺序天然随机），
// 混合场景（部分已探测、部分未探测）必须【每次都放行】。
func TestGuaranteedProbeDeterministicAcrossOrder(t *testing.T) {
	p := newModelTestPool(t)
	// 5 个已探测过 + 1 个未探测（稳态中夹一个新账号的典型情形）
	for i := 0; i < 5; i++ {
		uid := fmt.Sprintf("probed%d", i)
		p.Add(&auth.Auth{UID: uid})
		p.NoteModelRateLimit(uid, "m", 10*time.Minute)
	}
	p.Add(&auth.Auth{UID: "fresh"})
	p.NoteModelRateLimit("fresh", "m", 10*time.Minute)

	p.mu.Lock()
	for uid, e := range p.byUID {
		st := e.modelCool["m"]
		st.lastHit = time.Now().Add(-(halfOpenQuiet + time.Minute))
		if uid != "fresh" {
			st.probeFails = 1
		}
	}
	p.mu.Unlock()
	// 概率门恒不命中：若判定错误地读到 fresh（probeFails=0）就会被挡
	p.SetRandomSource(func(n int64) int64 { return n })

	for i := 0; i < 200; i++ {
		if a := p.PickExcluding(nil, "m"); a == nil {
			t.Fatalf("第 %d 次被挡：存在已探测过的候选时必须放行（保底语义不得依赖 map 顺序）", i)
		}
	}
}

// TestSmallPoolMixedScenarioRegression 小规模混合场景（1 已探测 + 1 未探测），
// 这是审计中 15% 失效的最典型形态。
func TestSmallPoolMixedScenarioRegression(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "probed"})
	p.Add(&auth.Auth{UID: "fresh"})
	p.NoteModelRateLimit("probed", "m", 10*time.Minute)
	p.NoteModelRateLimit("fresh", "m", 10*time.Minute)
	p.mu.Lock()
	p.byUID["probed"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Minute))
	p.byUID["fresh"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Minute))
	p.byUID["probed"].modelCool["m"].probeFails = 1
	p.mu.Unlock()
	p.SetRandomSource(func(n int64) int64 { return n }) // 恒不命中概率门

	for i := 0; i < 200; i++ {
		if a := p.PickExcluding(nil, "m"); a == nil {
			t.Fatalf("第 %d 次被挡：已探测过 probeFails>0 的候选存在时必须放行", i)
		}
	}
}

// TestProbeFlagIsBooleanSemantics probe 标记是布尔语义：
// 无论放行多少次，标记只表示「探测过」，不随次数无限增长。
func TestProbeFlagIsBooleanSemantics(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // 必放行

	for i := 0; i < 500; i++ {
		p.mu.Lock()
		p.byUID["a1"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Minute))
		p.mu.Unlock()
		if a := p.PickExcluding(nil, "m"); a == nil {
			t.Fatalf("第 %d 次未放行", i)
		}
	}
	p.mu.RLock()
	n := p.byUID["a1"].modelCool["m"].probeFails
	p.mu.RUnlock()
	if n > 1 {
		t.Fatalf("探测标记应封顶为 1（布尔语义），实际 %d", n)
	}
}

// TestProbeSurvivesManyRounds 修复不得破坏保底节拍。
//
// 语义分层（刻意如此）：
//   - 首次探测（probeFails==0）走 10% 概率门防惊群——被挡属预期，不是缺陷
//     （pick 调用频繁，期望很快命中）；
//   - 一旦探测过（probeFails==1），此后每过安静期【必然】放行，不再受概率门影响。
//
// 本用例锁定第二层：先强制命中一次进入"已探测"状态，再让概率门恒不命中，
// 验证后续 10 轮仍全部放行（保底不受概率门随机性拖延）。
func TestProbeSurvivesManyRounds(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", 10*time.Minute)

	// 第一步：概率门恒命中 → 首次探测放行（进入 probeFails=1）
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.mu.Lock()
	p.byUID["a1"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
	p.mu.Unlock()
	if a := p.PickExcluding(nil, "m"); a == nil {
		t.Fatal("概率门恒命中时首次探测应放行")
	}
	p.NoteModelRateLimit("a1", "m", 10*time.Minute) // 首次探测失败

	// 第二步：概率门改恒不命中 → 后续每轮仍应放行（保底豁免概率门）
	p.SetRandomSource(func(n int64) int64 { return n })
	released := 0
	for round := 0; round < 10; round++ {
		p.mu.Lock()
		p.byUID["a1"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
		p.mu.Unlock()
		if a := p.PickExcluding(nil, "m"); a != nil {
			released++
			p.NoteModelRateLimit("a1", "m", 10*time.Minute) // 探测失败
		}
	}
	if released != 10 {
		t.Fatalf("已探测过之后 10 个安静期窗口应放行 10 次（保底不受概率门影响），实际 %d", released)
	}
}
