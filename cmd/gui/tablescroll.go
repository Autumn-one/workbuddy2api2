// tablescroll.go — TableView 重建时保持垂直滚动位置。
//
// 缺陷（用户报告）：账号列表「一直刷新，滚到下面立刻被弹回顶部」。
//
// 根因：refreshAccounts 每次 Replace → PublishRowsReset 都会让 walk 的
// TableView 重新 setItemCount；而表格内容变化（签到/积分/状态列）发生频繁，
// 每次重建后原生 ListView 的滚动位置丢失、回到顶部。恢复选中行用的
// LVM_ENSUREVISIBLE 只能保证该行可见，不会把视口精确弹回原来的滚动偏移，
// 于是用户体验就是「往下滚 → 一次刷新 → 又回顶部」。
//
// 修法：Rebuild 前记录（topIndex + 该行视口内偏移），重建后按同一像素偏移
// 精确滚回（LVM_SCROLL），从而滚动位置不随刷新漂移。
package main

import (
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// tvScrollPos 一次重建前的滚动快照：可视区第一行 + 该行的视口内偏移（像素）。
type tvScrollPos struct {
	topIndex int
	offsetY  int32
}

// tvCaptureScroll 记录 tv 当前的垂直滚动位置。
// offsetY = 视口第一行顶部到视口顶部的像素距离（可能为负：第一行被部分滚出）。
func tvCaptureScroll(tv *walk.TableView) tvScrollPos {
	hwnd := tv.Handle()
	top := int(win.SendMessage(hwnd, win.LVM_GETTOPINDEX, 0, 0))
	pos := tvScrollPos{topIndex: top}
	if top < 0 {
		pos.topIndex = 0
		return pos
	}
	// 视口第一行的矩形：其 Top 即相对视口顶部的偏移（部分滚出时为负）。
	rc := win.RECT{Left: win.LVIR_BOUNDS}
	if win.SendMessage(hwnd, win.LVM_GETITEMRECT, uintptr(top), uintptr(unsafe.Pointer(&rc))) != 0 {
		pos.offsetY = rc.Top
	}
	return pos
}

// tvRestoreScroll 把 tv 滚回 capture 时记录的（topIndex + offsetY）。
// 行数减少（账号被删）时 topIndex 越界，原生 LVM_GETITEMRECT 失败 → 退化为滚到底。
func tvRestoreScroll(tv *walk.TableView, pos tvScrollPos, rowCount int) {
	hwnd := tv.Handle()
	if rowCount <= 0 {
		return
	}
	top := pos.topIndex
	if top >= rowCount {
		top = rowCount - 1 // 账号变少：至少停在最后一行附近，不弹回顶
	}
	rc := win.RECT{Left: win.LVIR_BOUNDS}
	if win.SendMessage(hwnd, win.LVM_GETITEMRECT, uintptr(top), uintptr(unsafe.Pointer(&rc))) == 0 {
		return
	}
	rowH := rc.Bottom - rc.Top
	if rowH <= 0 {
		return
	}
	// 目标：让 top 行显示在「视口顶部 + offsetY」处。
	// 当前它在 rc.Top 处 → 需要的位移 = rc.Top - offsetY（向下为正）。
	dy := rc.Top - pos.offsetY
	if dy == 0 {
		return
	}
	win.SendMessage(hwnd, win.LVM_SCROLL, 0, uintptr(int32(dy)))
}
