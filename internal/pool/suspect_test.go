package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ─────────────── 疑似被上游拉黑（连续 11140 的结果）───────────────
//
// 背景（生产事故 2026-09-13）：部分账号被上游风控标记后【永久性】返回
// 11140（实测某账号 189 次全拒、成功 0 次），但网关只给 10 分钟冷却——
// 过期后又会被抽中，每次白撞一次上游往返；21 账号池里这类"死号"有 5~6 个，
// 导致 MaxRotate=3 的轮转很容易被它们耗光（02:57 / 03:07 两次 503）。
//
// 设计约束（重要）：不能用"发请求复检"来判定账号是否恢复——那会持续消耗
// 积分（用户的账号多为一次性，积分不可再生）。因此采用：
//
//	连续 N 次 11140 → 进入"疑似拉黑"，冷却时长显著拉长（默认 1h）；
//	冷却到期 → 自动清空计数，给它一次公平重试机会（自愈，零额外请求）；
//	重试仍被拒 → 重新累计、再次进入疑似拉黑；
//	也可由用户在 GUI 手动解除（立刻给机会，不必等冷却）。
const (
	testSuspectThreshold = 3
	testSuspectCooldown  = time.Hour
)

// TestContentRejectCounting 连续计数：未达阈值不进疑似拉黑。
func TestContentRejectCounting(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	for i := 1; i < suspectBanThreshold; i++ {
		_, suspected := p.NoteContentReject("a1", 10*time.Minute)
		if suspected {
			t.Fatalf("第 %d 次不应进入疑似拉黑（阈值 %d）", i, suspectBanThreshold)
		}
	}
	if p.IsSuspectedBanned("a1") {
		t.Error("未达阈值不应标记疑似拉黑")
	}
}

// TestContentRejectReachesThreshold 达阈值进入疑似拉黑 + 长冷却。
func TestContentRejectReachesThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	var suspected bool
	for i := 0; i < suspectBanThreshold; i++ {
		_, suspected = p.NoteContentReject("a1", 10*time.Minute)
	}
	if !suspected {
		t.Fatalf("连续 %d 次 11140 应进入疑似拉黑", suspectBanThreshold)
	}
	if !p.IsSuspectedBanned("a1") {
		t.Error("应标记为疑似拉黑")
	}
	// 冷却必须显著长于普通 10 分钟
	remain := time.Until(p.ModelCooldownUntil("a1", "")) // 账号级冷却
	_ = remain
	st, _ := p.Status("a1")
	if !st.Cooling {
		t.Fatal("疑似拉黑账号应处于冷却（不参与选号）")
	}
	if d := time.Until(st.Until); d < suspectBanCooldown-2*time.Minute {
		t.Errorf("疑似拉黑冷却 %v 应接近 %v（远长于普通 10 分钟）", d, suspectBanCooldown)
	}
}

// TestSuspectBanExcludedFromPick 疑似拉黑账号不参与选号（这是核心收益）。
func TestSuspectBanExcludedFromPick(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "bad"})
	p.Add(&auth.Auth{UID: "good"})
	for i := 0; i < suspectBanThreshold; i++ {
		p.NoteContentReject("bad", 10*time.Minute)
	}
	for i := 0; i < 50; i++ {
		a := p.PickExcluding(nil, "")
		if a == nil {
			t.Fatal("good 可用却未选出")
		}
		if a.UID == "bad" {
			t.Fatalf("疑似拉黑账号不应被选中（第 %d 次）", i)
		}
	}
}

// TestSuspectBanCountingSurvivesCooldownExpiry 关键回归（实测缺陷）：
// 计数【不得因冷却到期而清零】，否则阈值永远达不成、疑似拉黑判定形同虚设。
//
// 生产实证（2026-09-13）：某账号 14 次 11140，间隔恒为 10~20 分钟——
// 正是"冷却到期→被抽中→再拒绝→计数被重置"的循环，导致 189 次拒绝都没触发拉黑。
func TestSuspectBanCountingSurvivesCooldownExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	for i := 0; i < suspectBanThreshold; i++ {
		p.NoteContentReject("a1", 10*time.Minute)
	}
	if !p.IsSuspectedBanned("a1") {
		t.Fatal("前置条件：应已疑似拉黑")
	}
	// 冷却到期（只代表"给一次重试机会"，不代表账号恢复）
	p.AdvanceAccountCooldowns()
	if p.ContentRejectCount("a1") != suspectBanThreshold {
		t.Errorf("冷却到期不得清零计数: got %d want %d",
			p.ContentRejectCount("a1"), suspectBanThreshold)
	}
	// 到期后再被拒：计数继续累加，仍判疑似拉黑
	_, suspected := p.NoteContentReject("a1", 10*time.Minute)
	if !suspected {
		t.Error("到期后再拒应继续判定疑似拉黑（计数未被时间清零）")
	}
	if n := p.ContentRejectCount("a1"); n != suspectBanThreshold+1 {
		t.Errorf("计数应继续累加: got %d want %d", n, suspectBanThreshold+1)
	}
}

// TestSuspectBanOnlySuccessOrManualClears 清零的两个合法途径：
// ① NoteSuccess（账号真的恢复）② ClearSuspectBan（用户手动解除）。
func TestSuspectBanOnlySuccessOrManualClears(t *testing.T) {
	// ① 成功清零
	p1 := New("")
	p1.Add(&auth.Auth{UID: "a1"})
	p1.NoteContentReject("a1", 10*time.Minute)
	p1.NoteContentReject("a1", 10*time.Minute)
	p1.NoteSuccess("a1")
	if p1.ContentRejectCount("a1") != 0 {
		t.Error("成功应清零计数（账号确实恢复）")
	}
	// ② 手动解除清零
	p2 := New("")
	p2.Add(&auth.Auth{UID: "a1"})
	p2.NoteContentReject("a1", 10*time.Minute)
	p2.ClearSuspectBan("a1")
	if p2.ContentRejectCount("a1") != 0 {
		t.Error("手动解除应清零计数")
	}
	// 时间流逝不清零（已由上一个用例覆盖，这里再固化一次语义）
	p3 := New("")
	p3.Add(&auth.Auth{UID: "a1"})
	p3.NoteContentReject("a1", time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	p3.AdvanceAccountCooldowns()
	if p3.ContentRejectCount("a1") != 1 {
		t.Error("时间流逝不得清零计数")
	}
}

// TestClearSuspectBan 手动解除：立刻可再参与选号。
func TestClearSuspectBan(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	for i := 0; i < suspectBanThreshold; i++ {
		p.NoteContentReject("a1", 10*time.Minute)
	}
	p.ClearSuspectBan("a1")
	if p.IsSuspectedBanned("a1") {
		t.Error("手动解除后不应仍标记疑似拉黑")
	}
	st, _ := p.Status("a1")
	if st.Cooling {
		t.Error("手动解除应清除冷却（立刻可试）")
	}
	if st.ContentRejects != 0 {
		t.Errorf("手动解除应清零计数, got %d", st.ContentRejects)
	}
}

// TestNoteSuccessResetsRejectCount 任一次成功即清零（账号恢复的最强信号）。
func TestNoteSuccessResetsRejectCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1"})
	p.NoteContentReject("a1", 10*time.Minute)
	p.NoteContentReject("a1", 10*time.Minute)
	p.NoteSuccess("a1")
	if n := p.ContentRejectCount("a1"); n != 0 {
		t.Errorf("成功后计数应清零, got %d", n)
	}
}

// TestStatusExposesSuspectState GUI 需要看到疑似拉黑状态与计数。
func TestStatusExposesSuspectState(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a1", Nickname: "甲"})
	p.NoteContentReject("a1", 10*time.Minute)
	st, ok := p.Status("a1")
	if !ok {
		t.Fatal("no status")
	}
	if st.ContentRejects != 1 {
		t.Errorf("Status.ContentRejects=%d want 1", st.ContentRejects)
	}
	if st.SuspectedBanned {
		t.Error("未达阈值不应显示疑似拉黑")
	}
	for i := 1; i < suspectBanThreshold; i++ {
		p.NoteContentReject("a1", 10*time.Minute)
	}
	st2, _ := p.Status("a1")
	if !st2.SuspectedBanned {
		t.Error("达阈值应显示疑似拉黑")
	}
}

// TestSuspectThresholdAndCooldownSane 阈值与冷却时长必须合理。
func TestSuspectThresholdAndCooldownSane(t *testing.T) {
	if suspectBanThreshold < 2 {
		t.Errorf("阈值 %d 过小（单次抖动就拉黑，误伤风险高）", suspectBanThreshold)
	}
	if suspectBanCooldown < 10*time.Minute {
		t.Errorf("疑似拉黑冷却 %v 应显著长于普通 10 分钟冷却", suspectBanCooldown)
	}
	if suspectBanCooldown > 24*time.Hour {
		t.Errorf("疑似拉黑冷却 %v 过长（风控可能是临时的，长冷却会误伤）", suspectBanCooldown)
	}
	t.Logf("阈值=%d 次，疑似拉黑冷却=%v", suspectBanThreshold, suspectBanCooldown)
}
