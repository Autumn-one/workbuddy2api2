package main

import (
	"testing"

	"workbuddy2api/internal/pool"
)

// ─────────────── 账号表复选框（活跃号池）───────────────
//
// walk 的 TableView 复选框协议：model 实现 ItemChecker（Checked/SetChecked），
// 控件按【行索引】查询勾选状态。Rebuild（PublishRowsReset）后行序可能变化，
// 因此勾选状态必须按 UID 保存（model.active map），Replace 时按 Status.Active 重建。

// TestAccountModelImplementsItemChecker 必须实现 walk.ItemChecker 才能显示复选框。
func TestAccountModelImplementsItemChecker(t *testing.T) {
	m := &accountModel{}
	// walk.ItemChecker 要求：Checked(int) bool + SetChecked(int, bool) error
	var _ interface {
		Checked(int) bool
		SetChecked(int, bool) error
	} = m
}

// TestCheckedFollowsStatusActive 勾选状态以 pool 的 Status.Active 为权威（Replace 回填）。
func TestCheckedFollowsStatusActive(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{
		{UID: "u1", Active: true},
		{UID: "u2", Active: false}, // 用户在另一实例/上次运行里取消勾选的账号
		{UID: "u3", Active: true},
	})
	if !m.Checked(0) {
		t.Error("u1 应勾选（Active=true）")
	}
	if m.Checked(1) {
		t.Error("u2 应未勾选（Active=false）")
	}
	if !m.Checked(2) {
		t.Error("u3 应勾选（Active=true）")
	}
}

// TestCheckedSurvivesReorder 表格重建（行序变化）后勾选状态跟随 UID，不乱跳。
func TestCheckedSurvivesReorder(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{
		{UID: "u1", Active: true},
		{UID: "u2", Active: false},
		{UID: "u3", Active: true},
	})
	// 重排：u2 从第 1 行挪到第 2 行。
	m.Replace([]pool.Status{
		{UID: "u3", Active: true},
		{UID: "u1", Active: true},
		{UID: "u2", Active: false},
	})
	if m.Checked(2) {
		t.Error("重排后 u2（第 2 行）仍应未勾选")
	}
	if !m.Checked(0) || !m.Checked(1) {
		t.Error("重排后 u3/u1 仍应勾选")
	}
}

// TestSetCheckedFiresCallback 点击复选框 → 回调（写回池）触发且带对 UID。
func TestSetCheckedFiresCallback(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{
		{UID: "u1", Active: true},
		{UID: "u2", Active: true},
	})
	var gotUID string
	var gotActive bool
	calls := 0
	m.onActiveChange = func(uid string, active bool) {
		gotUID, gotActive = uid, active
		calls++
	}
	if err := m.SetChecked(1, false); err != nil {
		t.Fatalf("SetChecked: %v", err)
	}
	if calls != 1 || gotUID != "u2" || gotActive {
		t.Fatalf("回调应收到 (u2,false)×1, got (%q,%v)×%d", gotUID, gotActive, calls)
	}
	// 重复设置同一值不再触发回调（防 walk 重绘抖动导致重复写池）。
	_ = m.SetChecked(1, false)
	if calls != 1 {
		t.Fatalf("同值重复 SetChecked 不应再触发回调, calls=%d", calls)
	}
}

// TestSetCheckedOutOfRange 越界索引安全（walk 可能用陈旧索引查询）。
func TestSetCheckedOutOfRange(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{{UID: "u1", Active: true}})
	for _, i := range []int{-1, 5, 100} {
		if err := m.SetChecked(i, false); err != nil {
			t.Errorf("SetChecked(%d) 应安全返回 nil, got %v", i, err)
		}
		if !m.Checked(0) {
			t.Errorf("SetChecked(%d) 不应影响已有行", i)
		}
	}
}

// TestCheckedDefaultsTrueForNewAccount 新账号（map 中无记录）默认勾选，与 pool 默认一致。
func TestCheckedDefaultsTrueForNewAccount(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{{UID: "u1", Active: true}})
	// 模拟 Replace 前的瞬态（active map 尚未回填）：默认勾选。
	m.active = nil
	if !m.Checked(0) {
		t.Error("active map 为空时应默认勾选（新账号进号池）")
	}
}

// TestAccountStateInactiveLabel 未勾选账号的状态列提示（压过"正常"，不压故障信号）。
func TestAccountStateInactiveLabel(t *testing.T) {
	// 未勾选 + 无故障 → 显示未启用（而非"正常"，否则会误以为它还在轮换）。
	s := pool.Status{Active: false}
	if got := accountState(s); got != "未启用（复选框未勾选）" {
		t.Errorf("未勾选且无故障应显示未启用, got %q", got)
	}
	// 未勾选 + 冷却 → 冷却优先（冷却是"账号暂时不可用"的信号，比"用户没勾"信息量大）。
	s2 := pool.Status{Active: false, Cooling: true, CoolRemaining: 60}
	if got := accountState(s2); got == "未启用（复选框未勾选）" {
		t.Errorf("冷却中不应被未启用掩盖, got %q", got)
	}
	// 勾选 + 无故障 → 正常。
	s3 := pool.Status{Active: true}
	if got := accountState(s3); got != "正常" {
		t.Errorf("勾选且无故障应显示正常, got %q", got)
	}
}

// TestActivePoolSig 活跃集签名：只含勾选的 UID（顺序与输入一致）。
func TestActivePoolSig(t *testing.T) {
	items := []pool.Status{
		{UID: "u1", Active: true},
		{UID: "u2", Active: false},
		{UID: "u3", Active: true},
	}
	if got := activePoolSig(items); got != "u1,u3," {
		t.Errorf("activePoolSig=%q want %q", got, "u1,u3,")
	}
	// 全不勾 → 空签名。
	items[0].Active = false
	items[2].Active = false
	if got := activePoolSig(items); got != "" {
		t.Errorf("全不勾应得空签名, got %q", got)
	}
}
