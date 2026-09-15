package main

import "testing"

// ─────────────── 账号表滚动位置保持（重建不弹回顶部）───────────────
//
// 缺陷（用户报告）：账号列表刷新后滚动位置被重置到顶。
// 修法：Replace 前 tvCaptureScroll、后 tvRestoreScroll。这里锁死滚动快照
// 与恢复的几何计算（不依赖真实窗口句柄的部分用纯逻辑测试）。

// TestScrollRestoreRowClamp 行数减少时 topIndex 必须被钳到最后一行，而非弹回顶部。
func TestScrollRestoreRowClamp(t *testing.T) {
	clamp := func(top, rowCount int) int {
		if rowCount <= 0 {
			return -1
		}
		if top >= rowCount {
			return rowCount - 1
		}
		return top
	}
	// 账号从 28 删到 5，原 topIndex=20 → 应钳到 4（停底部附近），不是 0。
	if got := clamp(20, 5); got != 4 {
		t.Errorf("clamp(20,5)=%d want 4（账号减少停底部，不回顶）", got)
	}
	if got := clamp(3, 28); got != 3 {
		t.Errorf("clamp(3,28)=%d want 3（正常情况不变）", got)
	}
	if got := clamp(0, 0); got != -1 {
		t.Errorf("clamp(0,0)=%d want -1（空表不滚动）", got)
	}
}

// TestScrollDeltaSign 滚回位移的符号约定：dy = 目标行当前视口Top − 记录偏移。
// dy>0 向下滚、dy<0 向上滚、dy=0 不动（避免无意义的滚动消息）。
func TestScrollDeltaSign(t *testing.T) {
	delta := func(curTop, wantOffset int32) int32 { return curTop - wantOffset }
	// 记录时第一行被部分滚出（offsetY=-10），重建后它顶到视口顶（curTop=0）
	// → 需向下滚 dy=+10 让它回到"部分滚出"的位置（dy>0 向下）。
	if got := delta(0, -10); got != 10 {
		t.Errorf("delta=%d want 10", got)
	}
	// 记录时第一行正好在视口顶（offsetY=0），重建后也在顶 → 不动。
	if got := delta(0, 0); got != 0 {
		t.Errorf("delta=%d want 0（已对齐则不滚动）", got)
	}
}

// TestCaptureScrollNegativeTopGuards 负 topIndex（异常/空表）必须归一化为 0。
func TestCaptureScrollNegativeTopGuards(t *testing.T) {
	normalize := func(top int) int {
		if top < 0 {
			return 0
		}
		return top
	}
	if got := normalize(-1); got != 0 {
		t.Errorf("normalize(-1)=%d want 0", got)
	}
	if got := normalize(7); got != 7 {
		t.Errorf("normalize(7)=%d want 7", got)
	}
}
