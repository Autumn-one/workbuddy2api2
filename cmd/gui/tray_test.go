package main

import (
	"testing"

	"github.com/lxn/win"
)

// ─────────────── 托盘/主窗口显隐的窗口状态决策 ───────────────
//
// 缺陷（用户报告）：
//  1. 左键点托盘图标不弹主窗口（只有右键出菜单）；
//  2. 主窗口最小化后无法恢复——showWindow 走 walk 的 Show() →
//     FormBase.Show → setWindowVisible(true) → ShowWindow(SW_SHOWNA)。
//     SW_SHOWNA = "显示但不激活"，【不会还原最小化窗口】，所以点了没反应。
//
// 修复：显式按窗口当前状态选择 ShowWindow 命令（最小化 → SW_RESTORE）。
// 决策函数独立成纯函数，便于测试（真实 ShowWindow 需 GUI 环境）。

// TestShowCommandForState 窗口状态 → ShowWindow 命令的映射。
func TestShowCommandForState(t *testing.T) {
	cases := []struct {
		name      string
		minimized bool
		visible   bool
		want      int32
	}{
		{"最小化 → 必须 SW_RESTORE（还原）", true, true, win.SW_RESTORE},
		{"隐藏（收进托盘）→ SW_SHOWNORMAL（显示）", false, false, win.SW_SHOWNORMAL},
		{"已可见未最小化 → SW_SHOWNORMAL（保持置前）", false, true, win.SW_SHOWNORMAL},
		{"最小化且不可见 → 仍须 SW_RESTORE（边界：两者同时成立）", true, false, win.SW_RESTORE},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := showCommandFor(c.minimized, c.visible); got != c.want {
				t.Errorf("showCommandFor(min=%v,vis=%v)=%d want %d", c.minimized, c.visible, got, c.want)
			}
		})
	}
}

// TestShowCommandRestoreIsEssential SW_RESTORE 必须是处理最小化的唯一命令：
// 用 SW_SHOWNA（walk 的默认）处理最小化窗口是无效的——这正是本缺陷的根因。
func TestShowCommandRestoreIsEssential(t *testing.T) {
	got := showCommandFor(true, true)
	if got == win.SW_SHOWNA {
		t.Fatal("最小化窗口用 SW_SHOWNA 无效（不改动窗口状态），必须 SW_RESTORE")
	}
	if got != win.SW_RESTORE {
		t.Errorf("最小化应使用 SW_RESTORE, got %d", got)
	}
}
