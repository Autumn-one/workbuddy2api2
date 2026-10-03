// usagepage.go — 「用量」页：token 用量展示（账号×模型×日期）。
//
// 纯展示：只读 server.TokenUsageStore 的内存快照，不发任何上游请求、不改任何状态。
package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lxn/walk"

	"workbuddy2api/internal/server"
)

// usageDayAll 日期下拉框的"全部日期"选项。
const usageDayAll = "全部日期"

// 用量页视图：0=明细（账号×模型×日期） 1=按账号 2=按模型。
const (
	usageViewDetail = iota
	usageViewByAccount
	usageViewByModel
)

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
	view := a.usageView()

	a.syncUsageDays(st)

	var rows []usageRow
	switch view {
	case usageViewByAccount:
		rows = usageRowsFromAccounts(st, day)
	case usageViewByModel:
		rows = usageRowsFromModels(st, day)
	default:
		rows = usageRowsFromDetail(st, day)
	}
	a.usage.view = view
	// 签名防抖：数据/视图/日期都没变就不重建表格——tickLoop 每 1.5s 会调本函数，
	// 全量 Replace 会让表格反复重绘闪烁（实测"一会儿闪一会儿闪"）。
	sig := usageSignature(view, day, rows)
	if sig == a.lastUsageSig {
		if a.lblUsageTotal != nil {
			a.lblUsageTotal.SetText(usageTotalText(st, day, view))
		}
		return
	}
	a.lastUsageSig = sig
	a.usage.Replace(rows)
	// 视图切换后列数/列名变了：重建列
	a.rebuildUsageColumns(view)

	if a.lblUsageTotal != nil {
		a.lblUsageTotal.SetText(usageTotalText(st, day, view))
	}
}

// usageSignature 用量表内容指纹：视图+日期+全部行数据。
// 任一变化才重建表格；1.5s 的周期刷新在数据不变时零成本跳过。
func usageSignature(view int, day string, rows []usageRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%s;", view, day)
	for _, r := range rows {
		fmt.Fprintf(&b, "%s|%s|%s|%d|%d|%d|%d|%d|%d;",
			r.UID, r.Model, r.Day, r.In, r.Out, r.Think, r.Cached, r.Requests, r.Missing)
	}
	return b.String()
}

// usageView 当前视图（0/1/2）。
func (a *app) usageView() int {
	if a.cbUsageScope == nil {
		return usageViewDetail
	}
	return a.cbUsageScope.CurrentIndex()
}

// usageDayOptions 构造日期下拉项：0 号固定为「全部日期」，其后为用量明细里出现过的
// 日期（倒序，最新在前）。抽成纯函数是为了能离线断言「下拉项 ↔ 聚合日期」的对应关系。
func usageDayOptions(rows []server.TokenUsageRow) []string {
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Day != "" {
			seen[r.Day] = true
		}
	}
	days := make([]string, 0, len(seen))
	for d := range seen {
		days = append(days, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days))) // 最新在前
	return append([]string{usageDayAll}, days...)
}

// usageDayAt 把下拉框索引映射为聚合用的日期：0 号是「全部日期」→ 空串（不筛日期），
// 越界同样返回空串。索引必须与 usageDayOptions 的项一一对应——**界面显示哪一项，
// 就必须聚合哪一天**。
//
// 曾经的 bug：a.usageDays 只存了日期（不含「全部日期」）而下拉框含，索引少减 1，
// 导致选「显示为今天」的那一项实际聚合前一天（10-01 的界面给出 09-30 的数字）。
func usageDayAt(options []string, idx int) string {
	if idx <= 0 || idx >= len(options) {
		return ""
	}
	return options[idx]
}

// selectedUsageDay 返回当前选中的日期（"YYYY-MM-DD"）；"全部日期"返回空串。
func (a *app) selectedUsageDay() string {
	if a.cbUsageDay == nil {
		return ""
	}
	return usageDayAt(a.usageDays, a.cbUsageDay.CurrentIndex())
}

// syncUsageDays 用已有的日期列表刷新日期下拉框（最新在前），保留当前选择。
func (a *app) syncUsageDays(st *server.TokenUsageStore) {
	if a.cbUsageDay == nil {
		return
	}
	items := usageDayOptions(st.Rows())
	if strings.Join(items, ",") == strings.Join(a.usageDays, ",") {
		return // 选项未变，不重建（避免打断用户选择）
	}
	// a.usageDays 与下拉框的项保持平行（含 0 号「全部日期」），
	// 这样 selectedUsageDay 的索引换算不会再错位。
	a.usageDays = items
	if err := a.cbUsageDay.SetModel(items); err != nil {
		logf("用量页日期下拉框刷新失败: %v", err)
		return
	}
	a.cbUsageDay.SetCurrentIndex(0)
}

// rebuildUsageColumns 按视图重建用量表列（视图切换后列数/列名不同）。
func (a *app) rebuildUsageColumns(view int) {
	if a.tvUsage == nil {
		return
	}
	type col struct {
		title string
		width int
	}
	var cols []col
	switch view {
	case usageViewByAccount:
		cols = []col{{"账号", 200}, {"请求数", 80}, {"总 token", 110}, {"输入", 100}, {"输出", 100}, {"缓存输入", 100}, {"其中思考", 100}}
	case usageViewByModel:
		cols = []col{{"模型", 200}, {"请求数", 80}, {"总 token", 110}, {"输入", 100}, {"输出", 100}, {"缓存输入", 100}, {"其中思考", 100}}
	default:
		cols = []col{{"账号", 140}, {"模型", 160}, {"日期", 95}, {"请求数", 75}, {"总 token", 100}, {"输入", 90}, {"输出", 90}, {"缓存输入", 90}, {"其中思考", 90}}
	}
	cs := a.tvUsage.Columns()
	_ = cs.Clear()
	for _, c := range cols {
		tvc := walk.NewTableViewColumn()
		_ = tvc.SetTitle(c.title)
		_ = tvc.SetWidth(c.width)
		if c.title != "账号" && c.title != "模型" && c.title != "日期" {
			_ = tvc.SetAlignment(walk.AlignFar)
		}
		_ = cs.Add(tvc)
	}
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

// usageRowsFromModels 模型级汇总（对明细 sum，自然 rollup）。
func usageRowsFromModels(st *server.TokenUsageStore, day string) []usageRow {
	rows := st.ByModel()
	if day != "" {
		rows = st.ByModelForDay(day)
	}
	out := make([]usageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, usageRow{
			Model: r.Model, Day: day,
			In: r.In, Out: r.Out, Think: r.Think, Cached: r.Cached,
			Requests: r.Requests, Missing: r.Missing,
		})
	}
	return out
}

// usageTotalText 顶部汇总文案：以【总 token】为主（M/B 缩写），请求数次之。
// 总 token = 输入 + 输出（缓存/思考是其子集，不重复计）。
func usageTotalText(st *server.TokenUsageStore, day string, view int) string {
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
	viewName := "明细"
	switch view {
	case usageViewByAccount:
		viewName = "按账号"
	case usageViewByModel:
		viewName = "按模型"
	}
	total := t.In + t.Out
	text := fmt.Sprintf("%s · %s：总 token %s（输入 %s + 输出 %s）· 请求 %s",
		scope, viewName,
		formatTokenCount(total),
		formatTokenCount(t.In),
		formatTokenCount(t.Out),
		formatTokenCount(t.Requests))
	if t.Cached > 0 {
		text += fmt.Sprintf(" · 其中缓存输入 %s · 思考 %s",
			formatTokenCount(t.Cached), formatTokenCount(t.Think))
	}
	if t.Missing > 0 {
		text += fmt.Sprintf("（%s 次请求上游未返回 usage，token 未计入）", formatTokenCount(t.Missing))
	}
	return text
}
