// singleacct.go — 对【选中的单个账号】手动签到 / 刷新额度。
//
// 背景：原有按钮只能对全部账号批量操作（「手动签到全部」「刷新额度」），
// 需要针对单个账号（例如刚登录的新号、或怀疑有问题的账号）单独操作。
//
// 复用既有上游能力，不新增接口：
//   - 签到：upstream.Client.DailyCheckin；重复签到由 IsAlreadyCheckedIn 识别
//   - 额度：upstream.Client.UserResource → pool.SetCreditsReason（落入积分历史）
//
// 与批量版口径一致：结果写进「签到记录」表；非成功不计入失败统计的只有
// 「今天已签到」（上游对重复签到返回 400 code=10001，属正常状态而非错误）。
package main

import (
	"fmt"

	"workbuddy2api/internal/upstream"
)

// classifyCheckinResult 把签到结果归因为展示文案 + 详情。
//   - err == nil → 「成功」
//   - err 命中 IsAlreadyCheckedIn → 「今天已签到」（正常状态，不计失败）
//   - 其余 → 「失败」+ 原始错误详情
//
// 抽成纯函数：真实签到需要上游，但归因逻辑（尤其"已签到不算失败"）必须可离线测试。
func classifyCheckinResult(err error) (result string, detail string) {
	if err == nil {
		return "成功", ""
	}
	detail = err.Error()
	if upstream.IsAlreadyCheckedIn(detail) {
		return "今天已签到", detail
	}
	return "失败", detail
}

// doCheckinSelected 对选中账号手动签到（成功后顺带刷新额度，与启动补签口径一致）。
func (a *app) doCheckinSelected() {
	items := a.svc.Accounts()
	a.takeSelection(func(idx int) {
		if idx < 0 || idx >= len(items) {
			a.lblAccts2.SetText("请先在表格里选中一个账号")
			return
		}
		st := items[idx]
		name := a.displayName(st.UID)
		up := a.svc.Upstream()
		if up == nil {
			a.lblAccts2.SetText("服务未运行，无法签到")
			return
		}
		at := a.svc.AuthByUID(st.UID)
		if at == nil {
			a.lblAccts2.SetText(name + "：凭证不在池中（刷新一下账号页）")
			return
		}
		a.lblAccts2.SetText(fmt.Sprintf("正在为 %s 签到…", name))

		go func() {
			result, detail := classifyCheckinResult(up.DailyCheckin(at))
			a.addCheckinRow(st.UID, result, detail)

			// 签到成功/已签到都顺带刷新额度：复用同一次上游窗口语义，
			// 让用户点一次就能看到最新积分（与启动补签一致）。
			refreshed := false
			if result == "成功" || result == "今天已签到" {
				if remain, err := up.UserResource(at); err == nil {
					a.svc.SetCreditsReason(st.UID, remain, "签到（手动/单账号）")
					refreshed = true
				}
			}
			a.mw.Synchronize(func() {
				msg := fmt.Sprintf("%s：签到%s", name, result)
				if refreshed {
					msg += " · 已同步额度"
				}
				if detail != "" && result == "失败" {
					msg += "（" + firstLine(detail) + "）"
				}
				a.lblAccts2.SetText(msg)
				a.refreshAccounts()
			})
		}()
	})
}

// doRefreshCreditsSelected 对选中账号手动刷新额度。
func (a *app) doRefreshCreditsSelected() {
	items := a.svc.Accounts()
	a.takeSelection(func(idx int) {
		if idx < 0 || idx >= len(items) {
			a.lblAccts2.SetText("请先在表格里选中一个账号")
			return
		}
		st := items[idx]
		name := a.displayName(st.UID)
		up := a.svc.Upstream()
		if up == nil {
			a.lblAccts2.SetText("服务未运行，无法刷新额度")
			return
		}
		at := a.svc.AuthByUID(st.UID)
		if at == nil {
			a.lblAccts2.SetText(name + "：凭证不在池中（刷新一下账号页）")
			return
		}
		a.lblAccts2.SetText(fmt.Sprintf("正在刷新 %s 的额度…", name))

		go func() {
			remain, err := up.UserResource(at)
			if err == nil {
				// 走 SetCreditsReason：写入积分历史（来源标注"手动刷新（单账号）"），
				// 便于在积分记录里区分批量刷新与单账号刷新。
				a.svc.SetCreditsReason(st.UID, remain, "手动刷新（单账号）")
			}
			a.mw.Synchronize(func() {
				if err != nil {
					a.lblAccts2.SetText(fmt.Sprintf("%s：刷新失败（%s）", name, firstLine(err.Error())))
				} else {
					a.lblAccts2.SetText(fmt.Sprintf("%s：额度 %d", name, remain))
				}
				a.refreshAccounts()
			})
		}()
	})
}
