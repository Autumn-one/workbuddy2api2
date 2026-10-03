package main

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/server"
)

// guiUsageTestStore 建一个不落盘的用量存储并写入样例。
func guiUsageTestStore(t *testing.T) *server.TokenUsageStore {
	t.Helper()
	st := server.NewTokenUsageStore("")
	t.Cleanup(st.Close)
	d1 := time.Date(2026, 9, 11, 10, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	st.Record("u1", "m1", d1, server.TokenDelta{In: 100, Out: 10, Cached: 60})
	st.Record("u1", "m1", d2, server.TokenDelta{In: 200, Out: 20, Cached: 10})
	st.Record("u1", "m2", d2, server.TokenDelta{In: 300, Out: 30, Cached: 5})
	st.Record("u2", "m1", d2, server.TokenDelta{In: 400, Out: 40})
	return st
}

// TestUsageRowsFromDetail 明细视图：每（账号×模型×日期）一行。
func TestUsageRowsFromDetail(t *testing.T) {
	st := guiUsageTestStore(t)
	all := usageRowsFromDetail(st, "")
	if len(all) != 4 {
		t.Fatalf("全部日期应 4 行, got %d", len(all))
	}
	oneDay := usageRowsFromDetail(st, "2026-09-12")
	if len(oneDay) != 3 {
		t.Fatalf("2026-09-12 应 3 行, got %d", len(oneDay))
	}
	for _, r := range oneDay {
		if r.Day != "2026-09-12" {
			t.Errorf("日期筛选失效: %+v", r)
		}
	}
}

// TestUsageRowsFromAccounts 账号级汇总：按账号 sum，且只 2 个账号。
func TestUsageRowsFromAccounts(t *testing.T) {
	st := guiUsageTestStore(t)
	rows := usageRowsFromAccounts(st, "")
	if len(rows) != 2 {
		t.Fatalf("账号级应 2 行, got %d", len(rows))
	}
	var u1 usageRow
	for _, r := range rows {
		if r.UID == "u1" {
			u1 = r
		}
	}
	if u1.In != 600 || u1.Out != 60 || u1.Cached != 75 {
		t.Errorf("u1 汇总错误: %+v", u1)
	}
	if u1.Requests != 3 {
		t.Errorf("u1 请求数=%d want 3", u1.Requests)
	}
	// 汇总行的 Model 应为空（界面渲染为「（账号汇总）」）
	if u1.Model != "" {
		t.Errorf("汇总行 Model 应为空, got %q", u1.Model)
	}

	// 指定日期
	dayRows := usageRowsFromAccounts(st, "2026-09-12")
	if len(dayRows) != 2 {
		t.Fatalf("单日账号级应 2 行, got %d", len(dayRows))
	}
	for _, r := range dayRows {
		if r.In == 0 {
			t.Errorf("单日汇总不应为 0: %+v", r)
		}
	}
}

// TestUsageTotalText 顶部汇总文案：以总 token 为主（M/B 缩写），请求数次之。
func TestUsageTotalText(t *testing.T) {
	st := guiUsageTestStore(t)
	txt := usageTotalText(st, "", usageViewDetail)
	// 总 token = 输入 1000 + 输出 100 = 1100 → 1.1K
	for _, want := range []string{"全部日期", "总 token 1.1K", "请求 4"} {
		if !strings.Contains(txt, want) {
			t.Errorf("文案缺 %q:\n%s", want, txt)
		}
	}
	// 单日 + 按账号视图
	txt2 := usageTotalText(st, "2026-09-12", usageViewByAccount)
	if !strings.Contains(txt2, "2026-09-12") || !strings.Contains(txt2, "按账号") {
		t.Errorf("单日/按账号文案错误:\n%s", txt2)
	}
}

// TestUsageTotalTextMissingHint 缺 usage 的请求要被明确标出，不能静默当 0。
func TestUsageTotalTextMissingHint(t *testing.T) {
	st := server.NewTokenUsageStore("")
	defer st.Close()
	st.Record("u1", "m1", time.Now(), server.TokenDelta{In: -1, Out: -1}) // 模拟失败请求无 usage
	txt := usageTotalText(st, "", usageViewDetail)
	if !strings.Contains(txt, "未返回 usage") {
		t.Errorf("应提示缺 usage 的请求数:\n%s", txt)
	}
}

// TestUsageDayOptionsMapping 日期下拉框的索引必须与聚合日期一一对应。
//
// 回归点（实测）：下拉框显示 2026-10-01，界面标签却给出 2026-09-30 的 78.63M——
// 因为 a.usageDays 不含「全部日期」而下拉框含，索引换算少减 1，整列日期全部前移一天。
func TestUsageDayOptionsMapping(t *testing.T) {
	st := guiUsageTestStore(t)
	opts := usageDayOptions(st.Rows())
	want := []string{"全部日期", "2026-09-12", "2026-09-11"}
	if len(opts) != len(want) {
		t.Fatalf("下拉项=%v want %v", opts, want)
	}
	for i := range want {
		if opts[i] != want[i] {
			t.Fatalf("下拉项[%d]=%q want %q（最新在前）", i, opts[i], want[i])
		}
	}

	// 0 号「全部日期」= 不筛日期
	if got := usageDayAt(opts, 0); got != "" {
		t.Errorf("「全部日期」应返回空串（不筛日期）, got %q", got)
	}
	// 不变式：下拉框显示哪一项，就聚合哪一天
	for i := 1; i < len(opts); i++ {
		if got := usageDayAt(opts, i); got != opts[i] {
			t.Errorf("索引 %d 显示 %q 却聚合 %q", i, opts[i], got)
		}
	}
	// 最新一天（下拉框第 2 项）
	if got := usageDayAt(opts, 1); got != "2026-09-12" {
		t.Errorf("选最新日应聚合 2026-09-12, got %q", got)
	}
	// 最早一天必须可达：老实现把它落进越界分支，退回「全部日期」
	if got := usageDayAt(opts, len(opts)-1); got != "2026-09-11" {
		t.Errorf("选最早日应聚合 2026-09-11, got %q", got)
	}
	// 越界安全
	if got := usageDayAt(opts, len(opts)); got != "" {
		t.Errorf("越界应返回空串, got %q", got)
	}

	// 端到端：映射结果喂给汇总文案，数字必须是那一天的
	// （2026-09-12：输入 200+300+400=900，输出 20+30+40=90，总 token 990）
	txt := usageTotalText(st, usageDayAt(opts, 1), usageViewDetail)
	if !strings.Contains(txt, "2026-09-12") || !strings.Contains(txt, "总 token 990") {
		t.Errorf("选最新日应显示当天汇总, got: %s", txt)
	}
}

// TestUsageRowsFromModels 模型级汇总：按模型聚合，请求/token 正确。
func TestUsageRowsFromModels(t *testing.T) {
	st := guiUsageTestStore(t)
	rows := usageRowsFromModels(st, "")
	if len(rows) == 0 {
		t.Fatal("按模型汇总不应为空")
	}
	// 模型汇总行的 UID 应为空（只有 Model 有值）
	for _, r := range rows {
		if r.Model == "" {
			t.Errorf("模型汇总行 Model 不应为空: %+v", r)
		}
		if r.Requests == 0 {
			t.Errorf("模型汇总行请求数不应为 0: %+v", r)
		}
	}
}

// TestFormatTokenCount M/B/K 缩写。
func TestFormatTokenCount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1K"}, {1234, "1.23K"},
		{1_000_000, "1M"}, {1_500_000, "1.5M"}, {2_345_678, "2.35M"},
		{1_000_000_000, "1B"}, {1_200_000_000, "1.2B"},
	}
	for _, c := range cases {
		if got := formatTokenCount(c.in); got != c.want {
			t.Errorf("formatTokenCount(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestUsageSignatureStable 防抖签名：同数据/视图/日期应相同，任一变化应不同。
func TestUsageSignatureStable(t *testing.T) {
	rows := []usageRow{
		{UID: "u1", Model: "m1", Day: "2026-09-12", In: 100, Out: 50, Requests: 2},
	}
	s1 := usageSignature(usageViewDetail, "", rows)
	s2 := usageSignature(usageViewDetail, "", rows)
	if s1 != s2 {
		t.Error("同数据签名应相同（否则会无谓重建表格闪烁）")
	}
	// 视图变 → 签名变
	if s1 == usageSignature(usageViewByAccount, "", rows) {
		t.Error("视图变化应改变签名")
	}
	// 日期变 → 签名变
	if s1 == usageSignature(usageViewDetail, "2026-09-12", rows) {
		t.Error("日期变化应改变签名")
	}
	// 数据变 → 签名变
	rows2 := []usageRow{{UID: "u1", Model: "m1", Day: "2026-09-12", In: 101, Out: 50, Requests: 2}}
	if s1 == usageSignature(usageViewDetail, "", rows2) {
		t.Error("数据变化应改变签名")
	}
}

// TestUsageModelRendersRows 表格列映射：明细视图（账号×模型×日期+总token M/B 缩写）。
func TestUsageModelRendersRows(t *testing.T) {
	m := &usageModel{view: usageViewDetail}
	m.Replace([]usageRow{
		{UID: "木瓜", Model: "glm-5.2", Day: "2026-09-12", Requests: 5, Cached: 800, In: 1000, Out: 50, Think: 20},
	})
	if m.RowCount() != 1 {
		t.Fatalf("行数=%d", m.RowCount())
	}
	// 明细列：账号|模型|日期|请求|总token|输入|输出|缓存|思考
	// 总 token = 1000+50 = 1050 → 1.05K
	want := []string{"木瓜", "glm-5.2", "2026-09-12", "5", "1.05K", "1K", "50", "800", "20"}
	for col, w := range want {
		if got := m.Value(0, col); got != w {
			t.Errorf("col %d=%v want %q", col, got, w)
		}
	}
	// 越界安全
	if got := m.Value(9, 0); got != "" {
		t.Errorf("越界应返回空, got %v", got)
	}
}

// TestUsageModelRendersByAccount 按账号视图的列映射。
func TestUsageModelRendersByAccount(t *testing.T) {
	m := &usageModel{view: usageViewByAccount}
	m.Replace([]usageRow{
		{UID: "木瓜", Requests: 12, In: 2_000_000, Out: 500_000, Cached: 100_000, Think: 50_000},
	})
	// 按账号列：账号|请求|总token|输入|输出|缓存|思考
	// 总 token = 2.5M
	want := []string{"木瓜", "12", "2.5M", "2M", "500K", "100K", "50K"}
	for col, w := range want {
		if got := m.Value(0, col); got != w {
			t.Errorf("col %d=%v want %q", col, got, w)
		}
	}
}

// TestUsageModelRendersByModel 按模型视图的列映射。
func TestUsageModelRendersByModel(t *testing.T) {
	m := &usageModel{view: usageViewByModel}
	m.Replace([]usageRow{
		{Model: "glm-5.2", Requests: 3, In: 1_000_000, Out: 200_000},
	})
	// 按模型列：模型|请求|总token|输入|输出|缓存|思考
	want := []string{"glm-5.2", "3", "1.2M", "1M", "200K", "0", "0"}
	for col, w := range want {
		if got := m.Value(0, col); got != w {
			t.Errorf("col %d=%v want %q", col, got, w)
		}
	}
}
