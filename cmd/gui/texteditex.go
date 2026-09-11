// texteditex.go — walk TextEdit 未封装的滚动能力。
//
// 需求背景：界面日志框每 1.5s 追加新行，旧行为 ScrollToCaret（EM_SCROLLCARET）
// 会把光标所在行的【最右端】滚进视口——最后一行是 6004 报文等长行时，
// 横向滚动条被甩到最右，下一轮又弹回，表现为"横向滚动条乱跳"。
// 这里提供两个只影响垂直方向的原语：
//   - teVScrollAtBottom：视口是否停在垂直底部（用于"贴底才跟随"判定）
//   - teScrollToBottom：垂直滚到底（WM_VSCROLL/SB_BOTTOM），不碰水平位置
package main

import (
	"github.com/lxn/walk"
	"github.com/lxn/win"
	"unsafe"
)

// vBottomTolerancePx 底部判定容差：规避像素级抖动导致的误判。
const vBottomTolerancePx = 2

// teVScrollAtBottom 报告 TextEdit 的垂直视口是否停在底部。
// 判定：当前位置 + 可见页高 ≥ 最大滚动位置 - 容差。
// 多行 EDIT 控件的垂直滚动单位是【行】，GetScrollInfo 的 NPage/NPos 亦按行计。
func teVScrollAtBottom(te *walk.TextEdit) bool {
	if te == nil {
		return true
	}
	var si win.SCROLLINFO
	si.CbSize = uint32(unsafe.Sizeof(si))
	si.FMask = win.SIF_RANGE | win.SIF_PAGE | win.SIF_POS
	if !win.GetScrollInfo(te.Handle(), win.SB_VERT, &si) {
		// 拿不到滚动信息（控件尚未创建/无滚动条）：保守视为贴底，保持跟随行为。
		return true
	}
	// 无滚动条（内容不足一屏）：必然在底部。
	if si.NMax-int32(si.NPage) <= 0 {
		return true
	}
	maxPos := si.NMax - int32(si.NPage)
	return si.NPos >= maxPos-vBottomTolerancePx
}

// teScrollToBottom 把 TextEdit 垂直滚到底，不影响水平滚动位置。
// 用 WM_VSCROLL + SB_BOTTOM（而非 EM_SCROLLCARET）：后者会连带横向滚动。
func teScrollToBottom(te *walk.TextEdit) {
	if te == nil {
		return
	}
	te.SendMessage(win.WM_VSCROLL, win.SB_BOTTOM, 0)
}
