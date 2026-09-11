package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// withNoPickGapAndDeterministic 关闭防撞窗口并固定随机源，让「谁被选中」可断言。
func withNoPickGapAndDeterministic(t *testing.T) {
	t.Helper()
	withNoPickGap(t)
}

// modelCooldownBase 与生产同值的起步冷却（5 分钟），测试直接引用同一常量，
// 避免测试写死 60s 而生产改成 5 分钟时测试还绿着却测错了东西。
const modelCooldownBase = 5 * time.Minute

// newModelTestPool 建一个空池（不落盘：state_file 传空）。
func newModelTestPool(t *testing.T) *Pool {
	t.Helper()
	return New("")
}

// TestModelCooldownBlocksPickAndExpires 核心：某模型撞 6004 后，
// 该【账号×模型】对不可选，但同账号其他模型与到期后仍可选。
func TestModelCooldownBlocksPickAndExpires(t *testing.T) {
	withNoPickGapAndDeterministic(t)
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})

	d := p.NoteModelRateLimit("a1", "glm-5.3", modelCooldownBase)
	if d != modelCooldownBase {
		t.Fatalf("首次冷却=%v want %v", d, modelCooldownBase)
	}
	if !p.IsModelCooling("a1", "glm-5.3") {
		t.Fatal("起步冷却窗口内应视为冷却中")
	}
	if p.ModelCooldownUntil("a1", "glm-5.3").IsZero() {
		t.Fatal("冷却后应存在截止时间")
	}
	// 同账号其他模型不受影响（模型级，不是账号级——这是当初不冷却的理由）
	if p.IsModelCooling("a1", "glm-5.2") {
		t.Error("同账号其他模型不应被波及")
	}
	// 候选过滤语义
	if !p.ExcludedByModelCooldown("a1", "glm-5.3") {
		t.Error("候选过滤应排除该（账号,模型）")
	}
	if p.ExcludedByModelCooldown("a1", "glm-5.2") {
		t.Error("其他模型不应被候选过滤排除")
	}
}

// TestModelCooldownExponentialBackoffCapsAtMax 连续撞墙：5m → 10m → 20m → 30m（封顶）。
// 用户明确要求：起步 5 分钟，封顶 30 分钟。
func TestModelCooldownExponentialBackoffCapsAtMax(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	const base = modelCooldownBase
	d := p.NoteModelRateLimit("a1", "glm-5.3", base)
	if d != base {
		t.Fatalf("首次=%v want %v", d, base)
	}
	// 300 → 600 → 1200 → 1800（封顶）
	want := []time.Duration{600, 1200, 1800, 1800, 1800, 1800, 1800, 1800, 1800, 1800}
	for i, w := range want {
		if got := p.NoteModelRateLimit("a1", "glm-5.3", base); got != time.Duration(w)*time.Second {
			t.Fatalf("第 %d 次=%v want %vs", i+1, got, w)
		}
	}
}

// TestModelCooldownCapsEvenWithLargerBase 封顶必须对任意基础时长生效，
// 不能因调用方传入更大的 base 而突破 30 分钟。
func TestModelCooldownCapsEvenWithLargerBase(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	got := time.Duration(0)
	for i := 0; i < 20; i++ {
		got = p.NoteModelRateLimit("a1", "m", 10*time.Minute)
	}
	if got > maxModelCooldown {
		t.Fatalf("连续撞墙 20 次后=%v 超过封顶 %v", got, maxModelCooldown)
	}
}

// TestModelCooldownResetOnSuccess 成功一次即清退避计数（信任恢复），
// 但【当前冷却窗口】本身不清零——它还没到期，到期由时间自然恢复。
func TestModelCooldownResetOnSuccess(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "glm-5.3", modelCooldownBase)
	p.NoteSuccess("a1")
	if p.ModelCooldownUntil("a1", "glm-5.3").IsZero() {
		t.Fatal("NoteSuccess 不应清掉未到期的冷却窗口")
	}
	if d := p.NoteModelRateLimit("a1", "glm-5.3", modelCooldownBase); d != modelCooldownBase {
		t.Fatalf("成功后重新撞墙应回到第一档 %v，got %v", modelCooldownBase, d)
	}
}

// TestModelCooldownBackoffSurvivesExpiry 到期恢复后，退避记忆保留：
// 再撞墙从【当前档】起步而非回到第一档（否则每次到期就撞一次永不升级）。
func TestModelCooldownBackoffSurvivesExpiry(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m", modelCooldownBase)
	p.AdvanceModelCooldowns(time.Now().Add(modelCooldownBase + time.Minute)) // 使 until 落到过去
	if p.IsModelCooling("a1", "m") {
		t.Fatal("到期后应视为已恢复")
	}
	if d := p.NoteModelRateLimit("a1", "m", modelCooldownBase); d != 2*modelCooldownBase {
		t.Fatalf("到期后再撞墙应按第二档 %v，got %v", 2*modelCooldownBase, d)
	}
}

// TestModelCooldownPickExcludesCooledPair 集成到选号：a1 的 glm-5.3 冷却中时，
// pick（换模型语义）不得选中 a1；a2 正常可选。
func TestModelCooldownPickExcludesCooledPair(t *testing.T) {
	withNoPickGapAndDeterministic(t)
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.Add(&auth.Auth{UID: "a2"})
	p.NoteModelRateLimit("a1", "glm-5.3", modelCooldownBase)
	for i := 0; i < 50; i++ {
		a := p.PickExcluding(nil, "glm-5.3")
		if a == nil {
			t.Fatal("a2 可选却选不出")
		}
		if a.UID == "a1" {
			t.Fatalf("a1 的 glm-5.3 冷却中却仍被选中（第 %d 次）", i)
		}
	}
}

// TestModelCooldownPerModelIsolation 不同模型的冷却互不影响（模型级而非账号级）。
func TestModelCooldownPerModelIsolation(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteModelRateLimit("a1", "m1", modelCooldownBase)
	if p.IsModelCooling("a1", "m2") {
		t.Error("m2 不应受 m1 冷却影响")
	}
	if p.IsModelCooling("a2", "m1") {
		t.Error("a2 不应受 a1 冷却影响")
	}
}

// TestModelCooldownDisabledAccountIgnored 已禁用账号不再累计退避（状态机归 Disable 管）。
func TestModelCooldownDisabledAccountIgnored(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	p.Disable("a1", "test")
	if d := p.NoteModelRateLimit("a1", "m", modelCooldownBase); d != 0 {
		t.Errorf("禁用账号应不记冷却（返回 0），got %v", d)
	}
}

// TestModelCooldownEmptyUIDOrModel 非法入参防御：空 uid/模型不产生任何状态。
func TestModelCooldownEmptyUIDOrModel(t *testing.T) {
	p := newModelTestPool(t)
	p.Add(&auth.Auth{UID: "a1"})
	if d := p.NoteModelRateLimit("", "m", 60*time.Second); d != 0 {
		t.Errorf("空 uid 应返回 0，got %v", d)
	}
	if d := p.NoteModelRateLimit("a1", " ", 60*time.Second); d != 0 {
		t.Errorf("空模型应返回 0，got %v", d)
	}
	if p.IsModelCooling("a1", "") {
		t.Error("空模型不应有冷却")
	}
}
