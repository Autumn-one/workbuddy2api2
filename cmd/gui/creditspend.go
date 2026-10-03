// creditspend.go — 「积分消耗」页：所有账号加总的每日积分消耗。
//
// 需求（用户）：要能一眼看到"每天一共消耗了多少积分"——不是单个账号的历史流水，
// 而是所有账号当天加总的一个数。
//
// 数据来源是已有的积分变动历史（credit-log.jsonl，见 creditlog.go）。本页只做
// 按天聚合，**不新增存储**：历史本身就是持久化的唯一事实来源，派生视图永远不会
// 与它不一致，也不需要额外的落盘/回滚逻辑。
//
// 口径（必须讲清楚，否则数字会被误读）：
//   - 消耗 = 当天所有账号【负向变动】之和的绝对值；增加 = 正向变动之和
//     （签到 +100、加量包、刷新修正都算增加，本页不区分来源）；
//   - 按【观测时刻】归日：积分下降是定时刷新"看到"的，不是发生时立刻记录的，
//     因此凌晨观察到的下降可能对应前一天的用量（最多滞后一个刷新间隔）；
//   - delta = 0 的「首次获取 / 无变化」不参与消耗与增加，也不计入变动次数。
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// creditDayRow 某一天所有账号加总的积分变动。
type creditDayRow struct {
	// Day "01-02"（与 creditRow.At 前 5 字符同格式）。不带年份：历史窗口
	// （上限 20000 条 ≈ 数月）远小于一年，跨年不会撞车。
	Day      string
	Spend    int64 // 消耗合计（负向变动绝对值）
	Gain     int64 // 增加合计（正向变动）
	Net      int64 // 净变化 = Gain - Spend
	Changes  int64 // 变动次数（delta != 0 的记录数）
	Accounts int   // 涉及账号数（当天有变动的不同 UID 数）
}

// creditDailyRows 把积分变动历史按天聚合（所有账号加总），返回日期倒序（最新在前）。
//
// 纯函数：可直接用构造的历史做离线断言，不需要 GUI、不需要上游。
func creditDailyRows(rows []creditRow) []creditDayRow {
	type bucket struct {
		row  creditDayRow
		uids map[string]bool
	}
	byDay := map[string]*bucket{}
	for _, r := range rows {
		if r.Delta == 0 {
			continue // 首次获取/无变化：不构成消耗或增加
		}
		day := creditDayKey(r.At)
		if day == "" {
			continue // 时间戳缺失/异常：宁可不统计，也不并进错误的日期
		}
		b, ok := byDay[day]
		if !ok {
			b = &bucket{row: creditDayRow{Day: day}, uids: map[string]bool{}}
			byDay[day] = b
		}
		if r.Delta < 0 {
			b.row.Spend += -r.Delta
		} else {
			b.row.Gain += r.Delta
		}
		b.row.Net += r.Delta
		b.row.Changes++
		if r.UID != "" {
			b.uids[r.UID] = true
		}
	}
	out := make([]creditDayRow, 0, len(byDay))
	for _, b := range byDay {
		r := b.row
		r.Accounts = len(b.uids)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out
}

// creditDayKey 从 "01-02 15:04:05" 取日期键 "01-02"；格式不符返回空串。
//
// 严格校验 "MM-DD"（第 3 字符是 '-'，其余 4 位是数字），不把任意字符串当日期——
// 历史文件里可能存在旧格式或手改过的记录，宁可漏统计到某一天，也不能把
// "1x-01" 这种东西变成一行"日期"。
func creditDayKey(at string) string {
	if len(at) < 5 || at[2] != '-' {
		return ""
	}
	for _, i := range []int{0, 1, 3, 4} {
		if at[i] < '0' || at[i] > '9' {
			return ""
		}
	}
	return at[:5]
}

// creditSpendSummaryText 顶部汇总文案：今日消耗 + 累计消耗（所有账号加总）。
func creditSpendSummaryText(daily []creditDayRow, today string) string {
	var totalSpend, totalGain, todaySpend, todayChanges int64
	for _, d := range daily {
		totalSpend += d.Spend
		totalGain += d.Gain
		if d.Day == today {
			todaySpend = d.Spend
			todayChanges = d.Changes
		}
	}
	text := fmt.Sprintf("今日（%s）消耗 %s · 累计消耗 %s",
		today, formatThousands(todaySpend), formatThousands(totalSpend))
	if todayChanges > 0 {
		text += fmt.Sprintf("（今日 %s 次变动）", formatThousands(todayChanges))
	}
	text += fmt.Sprintf(" · 累计增加 %s · 覆盖 %d 天",
		formatThousands(totalGain), len(daily))
	return text
}

// creditSpendSignature 表格内容指纹（防抖用）：数据没变就不重建表格。
func creditSpendSignature(daily []creditDayRow) string {
	var b strings.Builder
	for _, d := range daily {
		fmt.Fprintf(&b, "%s|%d|%d|%d|%d|%d;", d.Day, d.Spend, d.Gain, d.Net, d.Changes, d.Accounts)
	}
	return b.String()
}

// refreshCreditSpend 刷新「积分消耗」页（UI 线程调用）。
//
// 只读内存里的积分历史，不发任何上游请求、不改任何状态。
// 签名防抖与用量页同款：tick 每 1.5s 调一次，数据不变时零成本跳过。
func (a *app) refreshCreditSpend() {
	if a.creditSpend == nil {
		return
	}
	var rows []creditRow
	if a.creditLog != nil {
		rows = a.creditLog.Rows()
	}
	daily := creditDailyRows(rows)
	if sig := creditSpendSignature(daily); sig != a.lastCreditSpendSig {
		a.lastCreditSpendSig = sig
		a.creditSpend.Replace(daily)
	}
	if a.lblCreditSpend != nil {
		a.lblCreditSpend.SetText(creditSpendSummaryText(daily, time.Now().Format("01-02")))
	}
}

// creditSpendModel 「积分消耗」表格模型：每天一行（最新在前）。
type creditSpendModel struct {
	walk.TableModelBase
	items []creditDayRow
}

func (m *creditSpendModel) RowCount() int { return len(m.items) }

// Value 列映射：日期 | 消耗 | 增加 | 净变化 | 变动次数 | 涉及账号。
func (m *creditSpendModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.Day
	case 1:
		return formatThousands(r.Spend)
	case 2:
		return creditSignedText(r.Gain)
	case 3:
		return creditSignedText(r.Net)
	case 4:
		return itoa(r.Changes)
	case 5:
		return itoa(int64(r.Accounts))
	}
	return ""
}

// Replace 换数据并整表重建。
func (m *creditSpendModel) Replace(items []creditDayRow) {
	m.items = items
	m.PublishRowsReset()
}

// creditSignedText 带符号的千分位文案（0 → "0"，正数 → "+N"）。
func creditSignedText(n int64) string {
	if n > 0 {
		return "+" + formatThousands(n)
	}
	return formatThousands(n)
}
