package main

import (
	"testing"

	"workbuddy2api/internal/pool"
)

// ─────────────── 表格重建后保持选中行 ───────────────
//
// 缺陷（用户报告）：账号列表选中一行后，几秒后选中被刷没。
//
// 根因（walk 源码 tableview.go:724-733）：
//
//	tv.model.RowsReset().Attach(func() {
//	    tv.setItemCount()
//	    if ip, ok := tv.providedModel.(IDProvider); ok && tv.restoringCurrentItemOnReset {
//	        restoreCurrentItemOrFallbackToFirst(ip)   // 恢复选中
//	    } else {
//	        tv.SetCurrentIndex(-1)                    // 无条件清空选中
//	    }
//	})
//
// 即 PublishRowsReset 时，只有 model 实现了 walk.IDProvider（ID(index) any）
// 才会按 ID 恢复选中；否则 walk 直接清空。
// 本项目 accountModel 未实现该接口 → 任何一次 Replace 都丢选中。
//
// 上一轮把 InFlight 等高频字段移出签名只是【降低了重建频率】，
// 没有解决"一旦重建就丢选中"——积分刷新/账号增删/冷却变化仍会丢。

// TestAccountModelImplementsIDProvider 必须实现 walk.IDProvider 才能保持选中。
func TestAccountModelImplementsIDProvider(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{
		{UID: "u1", Nickname: "甲"},
		{UID: "u2", Nickname: "乙"},
	})
	// walk.IDProvider 要求：ID(index) interface{}
	var _ interface {
		ID(int) interface{}
	} = m
}

// TestAccountModelIDIsUID ID 必须是稳定标识（UID），且随行序变化仍指向同一账号。
func TestAccountModelIDIsUID(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{
		{UID: "u1"}, {UID: "u2"}, {UID: "u3"},
	})
	if got := m.ID(0); got != "u1" {
		t.Errorf("ID(0)=%v want u1", got)
	}
	if got := m.ID(2); got != "u3" {
		t.Errorf("ID(2)=%v want u3", got)
	}
	// 重排后 ID 仍跟随同一账号（这是"恢复选中"能工作的前提）
	m.Replace([]pool.Status{
		{UID: "u3"}, {UID: "u1"}, {UID: "u2"},
	})
	if got := m.ID(0); got != "u3" {
		t.Errorf("重排后 ID(0)=%v want u3", got)
	}
}

// TestAccountModelIDOutOfRange 越界必须安全（walk 可能用陈旧索引查询）。
func TestAccountModelIDOutOfRange(t *testing.T) {
	m := &accountModel{}
	m.Replace([]pool.Status{{UID: "u1"}})
	for _, i := range []int{-1, 1, 100} {
		if got := m.ID(i); got != nil {
			t.Errorf("ID(%d)=%v want nil（越界应返回 nil）", i, got)
		}
	}
	// 空模型
	m2 := &accountModel{}
	if got := m2.ID(0); got != nil {
		t.Errorf("空模型 ID(0)=%v want nil", got)
	}
}

// ─────────────── 为什么只实现 IDProvider 还不够 ───────────────
//
// walk 只在 SetCurrentIndex 路径维护 tv.currentItemID（tableview.go:1217）；
// 用户【鼠标点击】选中走的是 LVN_ITEMCHANGED（2350 行），不更新 currentItemID。
// 因此 currentItemID 可能一直是 nil → 重建时恢复循环 ip.ID(i)==nil 永不成立
// → fallback 到 SetCurrentIndex(0)，选中【跳到第一行】而不是用户选的那行。
//
// 结论：必须在应用层自己记录"用户选中的 UID"，重建后主动恢复。

// TestRememberAndRestoreSelection 记录选中 UID 并在行序变化后找回原行。
func TestRememberAndRestoreSelection(t *testing.T) {
	m := &accountModel{}
	rows := []pool.Status{{UID: "u1"}, {UID: "u2"}, {UID: "u3"}}

	// 用户选中 u2（记录 UID）
	saved := accountUIDAt(rows, 1)
	if saved != "u2" {
		t.Fatalf("应记录 u2, got %q", saved)
	}
	m.Replace(rows)

	// 行序变化（账号增删/重排）后，应能按 UID 找回 u2 的新位置
	reordered := []pool.Status{{UID: "u3"}, {UID: "u2"}, {UID: "u1"}}
	idx := indexOfAccountUID(reordered, saved)
	if idx != 1 {
		t.Fatalf("u2 新位置应 1, got %d", idx)
	}
}

// TestIndexOfAccountUIDMissing 账号被删除时返回 -1（调用方据此不恢复，避免误选）。
func TestIndexOfAccountUIDMissing(t *testing.T) {
	rows := []pool.Status{{UID: "u1"}, {UID: "u3"}}
	if got := indexOfAccountUID(rows, "u2"); got != -1 {
		t.Errorf("已删除账号应返回 -1, got %d", got)
	}
	if got := indexOfAccountUID(rows, ""); got != -1 {
		t.Errorf("空 UID 应返回 -1, got %d", got)
	}
	if got := indexOfAccountUID(nil, "u1"); got != -1 {
		t.Errorf("空列表应返回 -1, got %d", got)
	}
}

// TestAccountUIDAtBounds 越界返回空串（不 panic）。
func TestAccountUIDAtBounds(t *testing.T) {
	rows := []pool.Status{{UID: "u1"}}
	for _, i := range []int{-1, 5} {
		if got := accountUIDAt(rows, i); got != "" {
			t.Errorf("越界应返回空串, got %q", got)
		}
	}
	if got := accountUIDAt(rows, 0); got != "u1" {
		t.Errorf("正常索引应返回 u1, got %q", got)
	}
}
