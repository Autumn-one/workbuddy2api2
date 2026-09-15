package main

import (
	"testing"

	"workbuddy2api/internal/pool"
)

// ─────────────── 账号表刷新闸门（「一直在刷新」缺陷的回归测试）───────────────
//
// 历史 bug：refreshAccounts 用 tblSignature 与 a.lastSig 比对来决定是否重建，
// 却从不把新签名写回 lastSig —— 非空账号集的签名永远 ≠ ""，于是每 1.5s 的
// tick 都整表 Replace → PublishRowsReset：滚动位重置、选中行被清、整表闪白，
// 即用户看到的「账号列表一直在刷新」。闸门必须把签名写回才能让比对生效。

// TestAcctSigChangedStopsRepeatRebuild 同一签名第二次进入不应再判为「变了」。
// 若闸门不写回，第二次仍会 true → 每个 tick 都整表重建（本测试正是防它回归）。
func TestAcctSigChangedStopsRepeatRebuild(t *testing.T) {
	a := &app{}
	items := []pool.Status{
		{UID: "u1", Nickname: "一号", Active: true, Credits: 100},
		{UID: "u2", Nickname: "二号", Active: false, Credits: 50},
	}
	sig := a.tblSignature(items)
	if !a.acctSigChanged(sig) {
		t.Fatal("首次见到签名应判为变化（需要重建）")
	}
	for i := 0; i < 5; i++ {
		if a.acctSigChanged(sig) {
			t.Fatalf("第 %d 次同签名调用仍判为变化——lastSig 未写回，周期刷新会无限重建", i+2)
		}
	}
}

// TestAcctSigChangedOnActiveFlip 勾选状态进签名：复选框一勾一消必须触发重建
// （复选框的显示值来自模型，只有重建/重绘才反映新状态；Active 在签名里）。
func TestAcctSigChangedOnActiveFlip(t *testing.T) {
	a := &app{}
	items := []pool.Status{
		{UID: "u1", Active: true},
		{UID: "u2", Active: true},
	}
	_ = a.acctSigChanged(a.tblSignature(items))
	items[1].Active = false
	if !a.acctSigChanged(a.tblSignature(items)) {
		t.Error("Active 翻转后签名应变化（触发重建刷新复选框显示）")
	}
}

// TestAcctSigChangedOnCredits 积分变化进签名：签到/扣费后积分列要重建刷新。
func TestAcctSigChangedOnCredits(t *testing.T) {
	a := &app{}
	items := []pool.Status{{UID: "u1", Active: true, Credits: 100}}
	_ = a.acctSigChanged(a.tblSignature(items))
	items[0].Credits = 200
	if !a.acctSigChanged(a.tblSignature(items)) {
		t.Error("积分变化应触发重建")
	}
}

// TestAcctSigStableAcrossInFlight 高频字段（在途/峰值）刻意不进签名：
// 它们每次请求都在变，若进签名则每个 tick 都重建（回到"一直刷新"老路）。
// 这些字段靠 updateItems 的轻量重绘更新。
func TestAcctSigStableAcrossInFlight(t *testing.T) {
	a := &app{}
	items := []pool.Status{{UID: "u1", Active: true, InFlight: 0, InFlightPeak: 0}}
	sig1 := a.tblSignature(items)
	if !a.acctSigChanged(sig1) {
		t.Fatal("首次见到签名应判为变化")
	}
	items[0].InFlight = 3
	items[0].InFlightPeak = 5
	items[0].PeakActive = true
	if sig2 := a.tblSignature(items); sig1 != sig2 {
		t.Error("InFlight/峰值变化不应改变签名（高频字段靠重绘而非重建更新）")
	}
	if a.acctSigChanged(sig1) {
		t.Error("高频字段变化不应触发整表重建")
	}
}

// TestUpdateItemsKeepsRowMapping 轻量刷新只换快照：行序（List 按 UID 排序）与
// 勾选状态不变，索引→UID 对应关系保持，越界访问仍安全。
func TestUpdateItemsKeepsRowMapping(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{
		{UID: "u1", Active: true, InFlight: 0},
		{UID: "u2", Active: false, InFlight: 0},
	})
	// 快照推进：在途数变化（不进签名），Active 不变。
	m.updateItems([]pool.Status{
		{UID: "u1", Active: true, InFlight: 2},
		{UID: "u2", Active: false, InFlight: 0},
	})
	if got := m.Value(0, 6); got != "2" {
		t.Errorf("updateItems 后在途列应读到新值 2, got %v", got)
	}
	if m.Checked(1) {
		t.Error("u2 勾选状态不应被 updateItems 改变")
	}
	if _, ok := m.At(5); ok {
		t.Error("越界 At 应返回 ok=false")
	}
}
