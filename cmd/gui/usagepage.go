// usagepage.go — 「用量」页：token 用量展示（账号×模型×日期）。
//
// 纯展示：只读 server.TokenUsageStore 的内存快照，不发任何上游请求、不改任何状态。
package main

import (
	"fmt"
	"sort"
	"strings"

	"workbuddy2api/internal/server"
)

// usageDayAll 日期下拉框的"全部日期"选项。
const usageDayAll = "全部日期"

// refreshUsage 依据当前视图/日期选择刷新用量表（UI 线程调用）。
func (a *app) refreshUsage() {
	if a.usage == nil {
		return
	}
	st := a.svc.UsageStore()
	if st == nil {
		a.usage.Replace(nil)
		if a.lblUsageTotal != nil {
			a.lblUsageTotal.SetText("用量统计不可用（服务未启动过）")
		}
		return
	}
	day := a.selectedUsageDay()
	byAccount := a.selectedUsageScopeIsAccount()

	a.syncUsageDays(st)

	var rows []usageRow
	if byAccount {
		rows = usageRowsFromAccounts(st, day)
	} else {
		rows = usageRowsFromDetail(st, day)
	}
	a.usage.Replace(rows)

	if a.lblUsageTotal != nil {
		a.lblUsageTotal.SetText(usageTotalText(st, day, byAccount))
	}
}

// selectedUsageScopeIsAccount 报告当前是否选中「账号级汇总」视图。
func (a *app) selectedUsageScopeIsAccount() bool {
	if a.cbUsageScope == nil {
		return false
	}
	return a.cbUsageScope.CurrentIndex() == 1
}

// selectedUsageDay 返回当前选中的日期（"YYYY-MM-DD"）；"全部日期"返回空串。
func (a *app) selectedUsageDay() string {
	if a.cbUsageDay == nil {
		return ""
	}
	idx := a.cbUsageDay.CurrentIndex()
	if idx <= 0 || idx >= len(a.usageDays) {
		return ""
	}
	return a.usageDays[idx]
}

// syncUsageDays 用已有的日期列表刷新日期下拉框（最新在前），保留当前选择。
func (a *app) syncUsageDays(st *server.TokenUsageStore) {
	if a.cbUsageDay == nil {
		return
	}
	seen := map[string]bool{}
	for _, r := range st.Rows() {
		if r.Day != "" {
			seen[r.Day] = true
		}
	}
	days := make([]string, 0, len(seen))
	for d := range seen {
		days = append(days, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days))) // 最新在前

	if strings.Join(days, ",") == strings.Join(a.usageDays, ",") {
		return // 选项未变，不重建（避免打断用户选择）
	}
	a.usageDays = days
	items := append([]string{usageDayAll}, days...)
	if err := a.cbUsageDay.SetModel(items); err != nil {
		logf("用量页日期下拉框刷新失败: %v", err)
		return
	}
	a.cbUsageDay.SetCurrentIndex(0)
}

// usageRowsFromDetail 账号×模型明细。
func usageRowsFromDetail(st *server.TokenUsageStore, day string) []usageRow {
	rows := st.Rows()
	if day != "" {
		rows = st.RowsForDay(day)
	}
	out := make([]usageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, usageRow{
			UID: r.UID, Model: r.Model, Day: r.Day,
			In: r.In, Out: r.Out, Think: r.Think, Cached: r.Cached,
			Requests: r.Requests, Missing: r.Missing,
		})
	}
	return out
}

// usageRowsFromAccounts 账号级汇总（对明细 sum，自然 rollup）。
// 汇总行的「模型」「日期」列留空，由模型层渲染为「（账号汇总）」/「（全部日期）」。
func usageRowsFromAccounts(st *server.TokenUsageStore, day string) []usageRow {
	rows := st.ByAccount()
	if day != "" {
		rows = st.ByAccountForDay(day)
	}
	out := make([]usageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, usageRow{
			UID: r.UID, Day: day,
			In: r.In, Out: r.Out, Think: r.Think, Cached: r.Cached,
			Requests: r.Requests, Missing: r.Missing,
		})
	}
	return out
}

// usageTotalText 顶部汇总文案：总输入/输出/缓存/请求数。
// 含"缺 usage"提示，避免用户以为这些请求的 token 真的是 0。
func usageTotalText(st *server.TokenUsageStore, day string, byAccount bool) string {
	var t server.TokenUsageRow
	if day != "" {
		t = st.TotalForDay(day)
	} else {
		t = st.Total()
	}
	scope := "全部日期"
	if day != "" {
		scope = day
	}
	view := "账号×模型"
	if byAccount {
		view = "账号级"
	}
	text := fmt.Sprintf("合计（%s · %s）：请求 %s · 缓存输入 %s · 输入 %s · 输出 %s · 其中思考 %s",
		scope, view,
		formatThousands(t.Requests),
		formatThousands(t.Cached),
		formatThousands(t.In),
		formatThousands(t.Out),
		formatThousands(t.Think))
	if t.Missing > 0 {
		text += fmt.Sprintf("（%s 次请求上游未返回 usage，token 未计入）", formatThousands(t.Missing))
	}
	return text
}
