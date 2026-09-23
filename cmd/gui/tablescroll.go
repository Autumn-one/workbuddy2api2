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
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// tvNativeLV 返回 TableView 内部真正的 SysListView32 句柄。
//
// 为什么不能用 tv.Handle()：lxn/walk 的 TableView 自身是自定义容器窗口
// （tableViewWindowClass），真正的列表是它内部的两个子窗口——冻结列 LV 与
// 普通列 LV。对容器句柄发 LVM_* 消息只会落到 DefWindowProc 返回 0：
// 「账号表自愈」曾因此每 tick 读到行数 0 ≠ 数据行数，签名闸门被旁路、
// 整表每 1.5s 强制重建（日志刷屏 + 界面闪白）；滚动恢复也全程静默失效。
//
// 返回较宽者：无冻结列时冻结 LV 退化为 0 宽，普通 LV 即主显示区；
// 两个 LV 的行数始终一致（walk setItemCount 对两者都写），读行数取哪个都对。
func tvNativeLV(tv *walk.TableView) win.HWND {
	if tv == nil {
		return 0
	}
	var best win.HWND
	var bestW int32
	for h := win.GetWindow(tv.Handle(), win.GW_CHILD); h != 0; h = win.GetWindow(h, win.GW_HWNDNEXT) {
		var cls [64]uint16
		n, err := win.GetClassName(h, &cls[0], len(cls))
		if err != nil || n <= 0 {
			continue
		}
		if syscall.UTF16ToString(cls[:n]) != "SysListView32" {
			continue
		}
		var rc win.RECT
		if !win.GetWindowRect(h, &rc) {
			continue
		}
		if w := rc.Right - rc.Left; best == 0 || w > bestW {
			best, bestW = h, w
		}
	}
	return best
}

// tvScrollPos 一次重建前的滚动快照：可视区第一行 + 该行的视口内偏移（像素）。
type tvScrollPos struct {
	topIndex int
	offsetY  int32
}

// tvCaptureScroll 记录 tv 当前的垂直滚动位置。
// offsetY = 视口第一行顶部到视口顶部的像素距离（可能为负：第一行被部分滚出）。
func tvCaptureScroll(tv *walk.TableView) tvScrollPos {
	hwnd := tvNativeLV(tv)
	if hwnd == 0 {
		return tvScrollPos{}
	}
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
	hwnd := tvNativeLV(tv)
	if hwnd == 0 || rowCount <= 0 {
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
