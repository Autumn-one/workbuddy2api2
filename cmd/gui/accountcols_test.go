package main

import (
	"strings"
	"testing"

	"workbuddy2api/internal/pool"
)

// ─────────────── 账号表列映射 ───────────────
//
// 背景（本次引入缺陷）：新增「拒审」列时插到了「优先级」之后，但 Value() 的
// case 索引没同步移动，导致列序与取值错位（优先级列显示拒审次数）。
// 列序与 case 索引是两处独立维护的，必须用测试锁死。
//
// 约定列序（与 ui.go 的 Columns 定义一致）：
//
//	0 昵称 | 1 UID | 2 积分 | 3 状态 | 4 优先级 | 5 拒审 | 6 在途 | 7 成功/总

// TestAccountModelColumnMapping 逐列验证取值来源，锁死索引与列序一致。
func TestAccountModelColumnMapping(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{{
		UID: "u1", Nickname: "甲", Credits: 500, CreditsKnown: true,
		Priority: 3, ContentRejects: 2,
		InFlight: 1, InFlightPeak: 4, PeakActive: true,
		SuccessCount: 10, ErrTotal: 2,
	}})

	cases := []struct {
		col  int
		want string
		desc string
	}{
		{0, "甲", "昵称"},
		{1, "u1", "UID"},
		{2, "500", "积分"},
		{4, "×3", "优先级（不是拒审次数！）"},
		{5, "2", "拒审次数（未达阈值只显示数字）"},
		{7, "10/12", "成功/总"},
	}
	for _, c := range cases {
		got, _ := m.Value(0, c.col).(string)
		if got != c.want {
			t.Errorf("列 %d（%s）=%q want %q", c.col, c.desc, got, c.want)
		}
	}
}

// TestAccountModelPriorityNotConfusedWithRejects 关键回归：优先级与拒审不得串列。
func TestAccountModelPriorityNotConfusedWithRejects(t *testing.T) {
	m := &accountModel{}
	// 优先级 7、拒审 3（且达阈值）
	m.Replace([]pool.Status{{
		UID: "u1", Priority: 7, ContentRejects: suspectThresholdForTest,
		SuspectedBanned: true, // 该字段由 pool 依据计数+冷却算出，此处显式设置
	}})
	prio, _ := m.Value(0, 4).(string)
	rej, _ := m.Value(0, 5).(string)
	if prio != "×7" {
		t.Errorf("优先级列=%q want ×7", prio)
	}
	if !strings.Contains(rej, "疑似拉黑") {
		t.Errorf("拒审列应标出疑似拉黑, got %q", rej)
	}
	if strings.Contains(prio, "疑似") {
		t.Error("优先级列混入了拒审文案（列错位）")
	}
}

// TestAccountStateShowsSuspectedBan 状态列应明确显示疑似拉黑（优先级高于普通冷却）。
func TestAccountStateShowsSuspectedBan(t *testing.T) {
	s := pool.Status{SuspectedBanned: true, ContentRejects: 3, Cooling: true}
	got := accountState(s)
	if !strings.Contains(got, "疑似") {
		t.Errorf("状态列应显示疑似拉黑: %q", got)
	}
	// 禁用仍然优先于一切（账号不可用是最强信号）
	s2 := pool.Status{Disabled: true, SuspectedBanned: true}
	if got := accountState(s2); !strings.Contains(got, "禁用") {
		t.Errorf("禁用应优先显示: %q", got)
	}
}

// 与 pool.suspectBanThreshold 保持一致的测试常量（避免跨包引用常量导致的耦合）。
const suspectThresholdForTest = 3
