package main

import (
	"strings"
	"testing"
)

// ─────────────── 「积分消耗」页：所有账号加总的每日消耗 ───────────────
//
// 需求（用户）：要能一眼看到"每天一共消耗了多少积分"（所有账号加总）。
// 数据来自积分变动历史，因此聚合逻辑必须可离线断言——不依赖 GUI/上游。

// creditSpendFixture 构造一段形状与真实历史一致的数据（含首次获取、签到、消耗）。
func creditSpendFixture() []creditRow {
	return []creditRow{
		// 10-01：两个账号，消耗 300 + 50；其中一个签到 +100
		{At: "10-01 09:00:00", UID: "u1", Old: 1000, New: 700, Delta: -300, Reason: "自动刷新"},
		{At: "10-01 12:00:00", UID: "u2", Old: 500, New: 450, Delta: -50, Reason: "自动刷新"},
		{At: "10-01 08:00:00", UID: "u1", Old: 600, New: 700, Delta: 100, Reason: "签到"},
		// 09-30：单账号消耗 20
		{At: "09-30 23:00:00", UID: "u1", Old: 1020, New: 1000, Delta: -20, Reason: "自动刷新"},
		// 首次获取余额：delta=0，不构成消耗/增加，也不该建出一行
		{At: "09-30 07:00:00", UID: "u3", Old: 0, New: 88, Delta: 0, First: true, Reason: "自动刷新"},
	}
}

// TestCreditDailyRowsAggregatesByDay 按天聚合：消耗/增加/净变化/次数/账号数，日期倒序。
func TestCreditDailyRowsAggregatesByDay(t *testing.T) {
	got := creditDailyRows(creditSpendFixture())
	if len(got) != 2 {
		t.Fatalf("应聚合出 2 天（首次获取不建行）, got %d: %+v", len(got), got)
	}
	// 最新在前
	if got[0].Day != "10-01" || got[1].Day != "09-30" {
		t.Fatalf("应按日期倒序, got %+v", got)
	}
	d := got[0]
	if d.Spend != 350 || d.Gain != 100 || d.Net != -250 {
		t.Errorf("10-01 应为 消耗350/增加100/净-250, got %+v", d)
	}
	if d.Changes != 3 || d.Accounts != 2 {
		t.Errorf("10-01 应为 3 次变动/2 个账号, got %+v", d)
	}
	e := got[1]
	if e.Spend != 20 || e.Gain != 0 || e.Net != -20 || e.Changes != 1 || e.Accounts != 1 {
		t.Errorf("09-30 聚合错误: %+v", e)
	}
}

// TestCreditDailyRowsSkipsBadTimestamps 时间戳缺失/异常的记录不并进任何日期
// （宁可不统计，也不能算到错误的某一天头上）。
func TestCreditDailyRowsSkipsBadTimestamps(t *testing.T) {
	rows := []creditRow{
		{At: "", UID: "u1", Delta: -5},
		{At: "bad", UID: "u1", Delta: -5},
		{At: "1x-01 00:00:00", UID: "u1", Delta: -5},
		{At: "10-01 00:00:00", UID: "u1", Delta: -7},
	}
	got := creditDailyRows(rows)
	if len(got) != 1 || got[0].Day != "10-01" || got[0].Spend != 7 {
		t.Fatalf("只应统计合法时间戳的那一条, got %+v", got)
	}
}

// TestCreditDailyRowsEmpty 没有历史（或只有首次获取）时返回空，不 panic。
func TestCreditDailyRowsEmpty(t *testing.T) {
	if got := creditDailyRows(nil); len(got) != 0 {
		t.Errorf("空历史应返回空, got %+v", got)
	}
	only := []creditRow{{At: "10-01 00:00:00", UID: "u1", Delta: 0, First: true}}
	if got := creditDailyRows(only); len(got) != 0 {
		t.Errorf("只有首次获取时不该建行, got %+v", got)
	}
}

// TestCreditSpendSummaryText 顶部汇总：今日消耗 + 累计消耗（所有账号加总）。
func TestCreditSpendSummaryText(t *testing.T) {
	txt := creditSpendSummaryText(creditDailyRows(creditSpendFixture()), "10-01")
	for _, want := range []string{
		"今日（10-01）消耗 350", "今日 3 次变动", "累计消耗 370", "累计增加 100", "覆盖 2 天",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("汇总文案缺 %q:\n%s", want, txt)
		}
	}
	// 今天还没有任何消耗时也必须给出 0，而不是漏掉今日字段
	txt2 := creditSpendSummaryText(creditDailyRows(creditSpendFixture()), "10-02")
	if !strings.Contains(txt2, "今日（10-02）消耗 0") {
		t.Errorf("无今日记录时应显示 0:\n%s", txt2)
	}
	if strings.Contains(txt2, "今日 3 次变动") {
		t.Errorf("今天没变动时不该提示变动次数:\n%s", txt2)
	}
}

// TestCreditSpendModelRendersRows 表格列映射：日期|消耗|增加|净变化|变动次数|涉及账号。
func TestCreditSpendModelRendersRows(t *testing.T) {
	m := &creditSpendModel{}
	m.Replace([]creditDayRow{
		{Day: "10-01", Spend: 1_234_567, Gain: 100, Net: -1_234_467, Changes: 42, Accounts: 7},
	})
	if m.RowCount() != 1 {
		t.Fatalf("行数=%d", m.RowCount())
	}
	want := []string{"10-01", "1,234,567", "+100", "-1,234,467", "42", "7"}
	for col, w := range want {
		if got := m.Value(0, col); got != w {
			t.Errorf("col %d=%v want %q", col, got, w)
		}
	}
	// 越界安全
	if got := m.Value(9, 0); got != "" {
		t.Errorf("越界应返回空, got %v", got)
	}
	if got := m.Value(0, 99); got != "" {
		t.Errorf("越界列应返回空, got %v", got)
	}
}

// TestCreditSpendSignatureStable 防抖签名：同数据相同、任一字段变化则不同
// （否则 tick 每 1.5s 会无谓重建表格闪烁）。
func TestCreditSpendSignatureStable(t *testing.T) {
	a := []creditDayRow{{Day: "10-01", Spend: 10, Gain: 1, Net: -9, Changes: 2, Accounts: 1}}
	b := []creditDayRow{{Day: "10-01", Spend: 10, Gain: 1, Net: -9, Changes: 2, Accounts: 1}}
	if creditSpendSignature(a) != creditSpendSignature(b) {
		t.Error("同数据签名应相同")
	}
	b[0].Spend = 11
	if creditSpendSignature(a) == creditSpendSignature(b) {
		t.Error("消耗变化应改变签名")
	}
}

// TestCreditSignedText 带符号文案：正数带 +，0 与负数原样。
func TestCreditSignedText(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{100, "+100"}, {0, "0"}, {-250, "-250"}, {1234567, "+1,234,567"},
	}
	for _, c := range cases {
		if got := creditSignedText(c.in); got != c.want {
			t.Errorf("creditSignedText(%d)=%q want %q", c.in, got, c.want)
		}
	}
}
