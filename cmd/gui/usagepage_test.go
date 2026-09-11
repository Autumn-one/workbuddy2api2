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

// TestUsageTotalText 顶部汇总文案：含请求数/输入/输出/缓存，且缺 usage 时明确提示。
func TestUsageTotalText(t *testing.T) {
	st := guiUsageTestStore(t)
	txt := usageTotalText(st, "", false)
	for _, want := range []string{"合计", "全部日期", "请求 4", "输入 1,000", "输出 100"} {
		if !strings.Contains(txt, want) {
			t.Errorf("文案缺 %q:\n%s", want, txt)
		}
	}
	// 单日 + 账号级视图
	txt2 := usageTotalText(st, "2026-09-12", true)
	if !strings.Contains(txt2, "2026-09-12") || !strings.Contains(txt2, "账号级") {
		t.Errorf("单日/账号级文案错误:\n%s", txt2)
	}
}

// TestUsageTotalTextMissingHint 缺 usage 的请求要被明确标出，不能静默当 0。
func TestUsageTotalTextMissingHint(t *testing.T) {
	st := server.NewTokenUsageStore("")
	defer st.Close()
	st.Record("u1", "m1", time.Now(), server.TokenDelta{In: -1, Out: -1}) // 模拟失败请求无 usage
	txt := usageTotalText(st, "", false)
	if !strings.Contains(txt, "未返回 usage") {
		t.Errorf("应提示缺 usage 的请求数:\n%s", txt)
	}
}

// TestUsageModelRendersRows 表格列映射（含汇总行占位文案）。
func TestUsageModelRendersRows(t *testing.T) {
	m := &usageModel{}
	m.Replace([]usageRow{
		{UID: "木瓜", Model: "glm-5.2", Day: "2026-09-12", Requests: 5, Cached: 800, In: 1000, Out: 50, Think: 20},
		{UID: "木瓜", Day: "", Requests: 5, In: 1000},
	})
	if m.RowCount() != 2 {
		t.Fatalf("行数=%d", m.RowCount())
	}
	want := []string{"木瓜", "glm-5.2", "2026-09-12", "5", "800", "1000", "50", "20"}
	for col, w := range want {
		if got := m.Value(0, col); got != w {
			t.Errorf("col %d=%v want %q", col, got, w)
		}
	}
	// 汇总行：模型与日期显示占位文案
	if got := m.Value(1, 1); got != "（账号汇总）" {
		t.Errorf("汇总行模型列=%v", got)
	}
	if got := m.Value(1, 2); got != "（全部日期）" {
		t.Errorf("汇总行日期列=%v", got)
	}
	// 越界安全
	if got := m.Value(9, 0); got != "" {
		t.Errorf("越界应返回空, got %v", got)
	}
}
