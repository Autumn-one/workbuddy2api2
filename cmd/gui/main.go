// WorkBuddy2API 桌面控制台（Windows / Go + walk）。
//
// 设计要点：网关服务与界面跑在同一个进程里，所以能随时启停、加完账号立即热加载，
// 不需要再单独拉起 wb2api.exe。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows/registry"

	"workbuddy2api/internal/appconfig"
	"workbuddy2api/internal/oauthflow"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/proxy"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
)

const (
	appName  = "WorkBuddy 网关"
	runKey   = "WorkBuddy2API"
	cfgFile  = "config.json"
	logLines = 800
)

// ─────────────────────────── 日志缓冲 ───────────────────────────

// logBuffer 线程安全的行缓冲：服务端 goroutine 写，UI 定时读。
type logBuffer struct {
	mu    sync.Mutex
	lines []string
	part  string
	// consumed 已被 UI 消费（Drain）掉的行数。lines 只保留最近 logLines 行，
	// 环形裁剪时同步回退，保证 Drain 语义始终是「上次之后的新行」。
	consumed int
}

// maxLogLineRunes 单行最大字符数。超长行（如 6004 上游报文近 300 字符）会撑大
// 日志框的横向滚动范围，且各行宽度差异让横向滚动条来回跳，写入前统一收敛。
const maxLogLineRunes = 160

// truncateLogLine 把超长行按 rune 截断并以省略号结尾；短行原样返回。
func truncateLogLine(s string) string {
	rs := []rune(s)
	if len(rs) <= maxLogLineRunes {
		return s
	}
	return string(rs[:maxLogLineRunes]) + "…"
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.part += string(p)
	for {
		i := strings.IndexByte(b.part, '\n')
		if i < 0 {
			break
		}
		b.lines = append(b.lines, truncateLogLine(b.part[:i]))
		b.part = b.part[i+1:]
	}
	if len(b.lines) > logLines {
		drop := len(b.lines) - logLines
		b.lines = b.lines[drop:]
		if b.consumed > drop {
			b.consumed -= drop
		} else {
			b.consumed = 0
		}
	}
	return len(p), nil
}

// Drain 返回自上次 Drain 之后新增的行（并推进消费游标）。
// UI 据此做【增量追加】而非全量 SetText——全量重设会重建 EDIT 控件的横向滚动
// 范围并触发 ScrollToCaret，导致横向滚动条乱跳、用户滚动位置被冲掉。
// 残行（无换行符）留在 part，待补齐换行后一起返回。
func (b *logBuffer) Drain() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.consumed >= len(b.lines) {
		return nil
	}
	out := b.lines[b.consumed:]
	b.consumed = len(b.lines)
	return out
}

func (b *logBuffer) Text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.lines)+1)
	out = append(out, b.lines...)
	if b.part != "" {
		out = append(out, b.part)
	}
	return strings.Join(out, "\r\n")
}

// ─────────────────────────── 表格模型 ───────────────────────────

type accountModel struct {
	walk.TableModelBase
	items []pool.Status
}

// modelRateRow 模型参数表一行（含思考深度等全部可用参数）。
type modelRateRow struct {
	ID    string
	Name  string
	Rate  string  // 消耗倍率显示（×0.51），无则 "—"
	RateV float64 // 数值倍率（供可用量估算）

	// 思考深度
	Effort     string // 默认思考档（defaultEffort / effort）
	Supported  string // 可选思考档（如 "low/high/max"），单档模型为 "—"
	CanDisable string // 能否关闭思考：是/否
	OnlyReason string // 只能推理：是/—

	// 容量
	ContextWindow int64
	MaxTokens     int64

	// 能力
	Images   string // 图片输入
	ToolCall string // 工具调用
	Reason   string // 推理能力

	// 元信息
	Vendor  string
	Tags    string
	IsDeflt string
	Desc    string
}

type modelRateModel struct {
	walk.TableModelBase
	items []modelRateRow
}

func (m *modelRateModel) RowCount() int { return len(m.items) }

func (m *modelRateModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.ID
	case 1:
		return r.Name
	case 2:
		return r.Effort
	case 3:
		return r.Supported
	case 4:
		return r.CanDisable
	case 5:
		return r.Rate
	case 6:
		return fmt.Sprintf("%d", r.ContextWindow)
	case 7:
		return fmt.Sprintf("%d", r.MaxTokens)
	case 8:
		return r.Images
	case 9:
		return r.ToolCall
	case 10:
		return r.Vendor
	case 11:
		return r.Desc
	default:
		return ""
	}
}

func (m *modelRateModel) Replace(items []modelRateRow) {
	m.items = items
	m.PublishRowsReset()
}

// Selected 返回当前选中行（用于详情面板）。
func (m *modelRateModel) At(i int) (modelRateRow, bool) {
	if i < 0 || i >= len(m.items) {
		return modelRateRow{}, false
	}
	return m.items[i], true
}

func (m *accountModel) RowCount() int { return len(m.items) }

func (m *accountModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	s := m.items[row]
	switch col {
	case 0:
		if s.Nickname == "" {
			return "（无昵称）"
		}
		return s.Nickname
	case 1:
		if len(s.UID) > 12 {
			return s.UID[:12] + "…"
		}
		return s.UID
	case 2:
		// 积分：0 与"未知"要区分开——从未刷新过额度时显示 "-" 而非 "0"，
		// 避免用户误以为"没积分了"。
		if s.Credits == 0 {
			return "-"
		}
		return fmt.Sprintf("%d", s.Credits)
	case 3:
		return accountState(s)
	case 4:
		// 优先级列：0 = 未设置（未标记），否则显示乘子（如 ×3）。
		if s.Priority > 0 {
			return fmt.Sprintf("×%g", s.Priority)
		}
		return "—"
	case 5:
		// 拒审列：连续内容安全拒绝次数；达阈值标记"疑似拉黑"（长冷却）。
		if s.SuspectedBanned {
			return fmt.Sprintf("%d 疑似拉黑", s.ContentRejects)
		}
		if s.ContentRejects > 0 {
			return fmt.Sprintf("%d", s.ContentRejects)
		}
		return "—"
	case 6:
		// 在途列展示"忙闲痕迹"：瞬时值优先（正在忙）；瞬时为 0 但峰值仍在可见窗口内时
		// 显示 "0（峰值 N）"，让整体落在 GUI 采样间隔之间的短请求也能被看见。
		if s.InFlight > 0 {
			if s.InFlightPeak > s.InFlight {
				return fmt.Sprintf("%d（峰 %d）", s.InFlight, s.InFlightPeak)
			}
			return fmt.Sprintf("%d", s.InFlight)
		}
		if s.PeakActive && s.InFlightPeak > 0 {
			return fmt.Sprintf("峰 %d", s.InFlightPeak)
		}
		return "-"
	case 7:
		if s.ErrTotal == 0 && s.SuccessCount == 0 {
			return "-"
		}
		return fmt.Sprintf("%d/%d", s.SuccessCount, s.SuccessCount+s.ErrTotal)
	}
	return ""
}

func (m *accountModel) Replace(items []pool.Status) {
	m.items = items
	m.PublishRowsReset()
}

// accountUIDAt 返回第 i 行的账号 UID；越界返回空串。
func accountUIDAt(rows []pool.Status, i int) string {
	if i < 0 || i >= len(rows) {
		return ""
	}
	return rows[i].UID
}

// indexOfAccountUID 在账号列表里查找 UID 所在行；找不到返回 -1。
func indexOfAccountUID(rows []pool.Status, uid string) int {
	if uid == "" {
		return -1
	}
	for i, r := range rows {
		if r.UID == uid {
			return i
		}
	}
	return -1
}

// ID 实现 walk.IDProvider：返回该行的稳定标识（账号 UID）。
//
// 为什么必须实现它（实测缺陷）：walk 的 TableView 在 model 发布 RowsReset 时
// （tableview.go:724）会判断 model 是否实现 IDProvider——
//   - 实现了 → 按 ID 恢复原来选中的行（restoreCurrentItemOrFallbackToFirst）；
//   - 没实现 → 无条件 SetCurrentIndex(-1)，即【清空选中】。
//
// 本项目此前未实现，导致任何一次表格重建（积分刷新、账号增删、冷却状态变化…）
// 都会把用户选中的行刷没。返回 UID 而非行号：行序可能变（账号增删/排序），
// 只有 UID 能稳定指向同一账号。
//
// 越界返回 nil（walk 可能用陈旧索引查询，必须安全）。
func (m *accountModel) ID(index int) interface{} {
	if index < 0 || index >= len(m.items) {
		return nil
	}
	return m.items[index].UID
}

// accountState 把账号状态压成一个人眼可读的短标签。
func accountState(s pool.Status) string {
	switch {
	case s.Disabled:
		// 禁用优先于一切：账号彻底不可用（需人工重登），此信号最强，
		// 不应被"疑似拉黑"等其他标记掩盖。
		return "已禁用（需重新登录）"
	case s.SuspectedBanned:
		// 其次才是风控标记（账号仍可能恢复，且可手动解除）。
		return "疑似被上游拉黑（连续内容安全拒绝）"
	case s.BreakerFails > 0 && !s.BreakerUntil.IsZero() && time.Now().Before(s.BreakerUntil):
		return fmt.Sprintf("熔断中 %s", untilText(s.BreakerUntil))
	case s.Cooling:
		if s.CoolRemaining > 0 {
			return fmt.Sprintf("冷却中 %s", shortDur(time.Duration(s.CoolRemaining)*time.Second))
		}
		return "冷却中"
	default:
		return "正常"
	}
}

func untilText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return shortDur(time.Until(t))
}

func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

type checkinRow struct {
	At     string
	UID    string
	Result string
	Detail string
}

// proxyBindingRow 代理绑定表一行（账号 → 出口节点）。
type proxyBindingRow struct {
	Name    string // 昵称（无则 UID 短）
	Node    string // 可读节点名（用户明确要求显示 Clash 里的名字）
	Port    int
	Region  string
	Healthy bool
}

type proxyBindingModel struct {
	walk.TableModelBase
	items []proxyBindingRow
}

func (m *proxyBindingModel) RowCount() int { return len(m.items) }

func (m *proxyBindingModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.Name
	case 1:
		return r.Node
	case 2:
		return itoa(int64(r.Port))
	case 3:
		return r.Region
	case 4:
		if r.Healthy {
			return "正常"
		}
		return "不通"
	default:
		return ""
	}
}

func (m *proxyBindingModel) Replace(items []proxyBindingRow) {
	m.items = items
	m.PublishRowsReset()
}

// usageRow 界面上的一行用量（账号级汇总或账号×模型明细）。
type usageRow struct {
	UID      string
	Model    string
	Day      string
	In       int64
	Out      int64
	Think    int64
	Cached   int64
	Requests int64
	Missing  int64
}

// usageModel 用量表模型：支持两种视图（账号×模型明细 / 账号级汇总）。
type usageModel struct {
	walk.TableModelBase
	items []usageRow
}

func (m *usageModel) RowCount() int { return len(m.items) }

func (m *usageModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.UID
	case 1:
		if r.Model == "" {
			return "（账号汇总）"
		}
		return r.Model
	case 2:
		if r.Day == "" {
			return "（全部日期）"
		}
		return r.Day
	case 3:
		return itoa(r.Requests)
	case 4:
		return itoa(r.Cached)
	case 5:
		return itoa(r.In)
	case 6:
		return itoa(r.Out)
	case 7:
		return itoa(r.Think)
	default:
		return ""
	}
}

func (m *usageModel) Replace(items []usageRow) {
	m.items = items
	m.PublishRowsReset()
}

type checkinModel struct {
	walk.TableModelBase
	items []checkinRow
}

// creditModel 「积分记录」表格模型。
//
// 支持按单个账号过滤：filterUID 非空且不是 creditAllUIDs 时，只展示该账号的记录。
// 原始数据全部保留在 items（最新在前），过滤只影响 Value/RowCount 的呈现，
// 因此切换过滤不需要回读磁盘，也不会丢失任何记录。
//
// 行内 UID 一律存【原始 UID】（与落盘一致），昵称在 Value() 里经 display 解析——
// 否则“按 UID 过滤”会与“显示的是昵称”相互矛盾，且昵称改名后过滤就会失效。
type creditModel struct {
	walk.TableModelBase
	items     []creditRow
	filterUID string // "" 或 creditAllUIDs = 全部账号
	// display 把原始 UID 解析为昵称；nil = 直接显示 UID。
	display func(uid string) string
}

// RowCount 报告当前（过滤后）可见行数。
func (m *creditModel) RowCount() int { return len(m.visible()) }

// visible 返回当前过滤条件下的行（按显示顺序，最新在前）。
func (m *creditModel) visible() []creditRow {
	if m.filterUID == "" || m.filterUID == creditAllUIDs {
		return m.items
	}
	out := make([]creditRow, 0, len(m.items))
	for _, r := range m.items {
		if creditMatchUID(r, m.filterUID) {
			out = append(out, r)
		}
	}
	return out
}

// Current 返回过滤后的第 row 条记录（供双击等交互使用）。
func (m *creditModel) Current(row int) (creditRow, bool) {
	vis := m.visible()
	if row < 0 || row >= len(vis) {
		return creditRow{}, false
	}
	return vis[row], true
}

// FilterUID 返回当前过滤的账号 UID（"" 或 creditAllUIDs = 全部）。
func (m *creditModel) FilterUID() string { return m.filterUID }

// SetFilter 切换账号过滤并刷新表格。filterUID 传 "" 或 creditAllUIDs 表示全部。
func (m *creditModel) SetFilter(uid string) {
	if m.filterUID == uid {
		return
	}
	m.filterUID = uid
	m.PublishRowsReset()
}

func (m *creditModel) Value(row, col int) interface{} {
	vis := m.visible()
	if row < 0 || row >= len(vis) {
		return ""
	}
	r := vis[row]
	switch col {
	case 0:
		return r.At
	case 1:
		if m.display != nil {
			return m.display(r.UID)
		}
		return r.UID
	case 2:
		return creditDeltaText(r)
	case 3:
		return creditRangeText(r)
	default:
		return r.Reason
	}
}

// Prepend 插入一行（最新在前）。上限与落盘口径一致（creditLogLimit）：
// 界面与落盘保持同一份可见范围，按账号过滤时不会因为"界面先截断"而看不到完整历史。
func (m *creditModel) Prepend(r creditRow) {
	m.items = append([]creditRow{r}, m.items...)
	if len(m.items) > creditLogLimit {
		m.items = m.items[:creditLogLimit]
	}
	m.PublishRowsReset()
}

// Replace 整体替换表格内容（启动时回填历史用）。
// 保留当前过滤条件：回填不应意外改变用户已经选定的账号视图。
func (m *creditModel) Replace(items []creditRow) {
	m.items = items
	m.PublishRowsReset()
}

func (m *checkinModel) RowCount() int { return len(m.items) }

func (m *checkinModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.At
	case 1:
		return r.UID
	case 2:
		return r.Result
	default:
		return r.Detail
	}
}

func (m *checkinModel) Prepend(r checkinRow) {
	m.items = append([]checkinRow{r}, m.items...)
	if len(m.items) > 300 {
		m.items = m.items[:300]
	}
	m.PublishRowsReset()
}

// Replace 整体替换表格内容（启动时回填历史记录用）。
func (m *checkinModel) Replace(items []checkinRow) {
	m.items = items
	m.PublishRowsReset()
}

// creditAllLabel 积分记录过滤下拉框里“全部账号”的显示文案。
const creditAllLabel = "全部账号"

// setCreditFilter 切换积分记录表格的账号过滤（并同步下拉框与提示文案）。
// uid 传 creditAllUIDs 或 "" 表示全部。必须在 UI 线程调用。
//
// 只切过滤、不回读磁盘：模型内保留完整历史，过滤仅影响呈现。
func (a *app) setCreditFilter(uid string) {
	if uid == "" {
		uid = creditAllUIDs
	}
	a.creditFilterUID = uid
	if a.credits != nil {
		a.credits.SetFilter(uid)
	}
	if a.cbCreditFilter != nil {
		// 同步下拉框选中项（若该账号不在下拉列表里则保持不动，只改表格）。
		if idx := indexOfStr(a.creditUIDs, uid); idx >= 0 && a.cbCreditFilter.CurrentIndex() != idx {
			_ = a.cbCreditFilter.SetCurrentIndex(idx)
		}
	}
	a.refreshCreditFilterLabel()
}

// refreshCreditFilterLabel 刷新积分记录区的计数提示。
func (a *app) refreshCreditFilterLabel() {
	if a.lblCreditFilter == nil || a.credits == nil {
		return
	}
	shown := a.credits.RowCount()
	total := len(a.credits.items)
	if a.creditFilterUID == "" || a.creditFilterUID == creditAllUIDs {
		a.lblCreditFilter.SetText(fmt.Sprintf("共 %d 条记录", total))
		return
	}
	a.lblCreditFilter.SetText(fmt.Sprintf("账号 %s：%d 条 / 共 %d 条", a.displayName(a.creditFilterUID), shown, total))
}

// onCreditFilterChanged 下拉框选择变化 → 切换过滤。
func (a *app) onCreditFilterChanged() {
	if a.cbCreditFilter == nil {
		return
	}
	idx := a.cbCreditFilter.CurrentIndex()
	if idx < 0 || idx >= len(a.creditUIDs) {
		return
	}
	a.setCreditFilter(a.creditUIDs[idx])
}

// syncCreditFilter 重建下拉框选项（账号增删后调用），并保持当前选中账号。
// 只重建真正变化的选择集，避免每帧重置下拉框（会打断用户操作）。
func (a *app) syncCreditFilter() {
	if a.cbCreditFilter == nil {
		return
	}
	names := []string{creditAllLabel}
	uids := []string{creditAllUIDs}
	for _, st := range a.svc.Accounts() {
		names = append(names, a.displayName(st.UID))
		uids = append(uids, st.UID)
	}
	if equalStrs(names, a.creditNames) && equalStrs(uids, a.creditUIDs) {
		a.refreshCreditFilterLabel()
		return
	}
	a.creditNames, a.creditUIDs = names, uids
	cur := a.creditFilterUID
	if cur == "" {
		cur = creditAllUIDs
	}
	idx := indexOfStr(uids, cur)
	if idx < 0 {
		// 被过滤的账号已被删除：回落"全部"，避免表格卡在空白状态。
		idx = 0
		cur = creditAllUIDs
	}
	if err := a.cbCreditFilter.SetModel(names); err != nil {
		log.Printf("积分过滤下拉框刷新失败: %v", err)
		return
	}
	_ = a.cbCreditFilter.SetCurrentIndex(idx)
	a.creditFilterUID = cur
	a.credits.SetFilter(cur)
	a.refreshCreditFilterLabel()
}

// onAccountActivated 账号表双击：直接看该账号的积分变化历史。
//
// 交互：切到「日志」页并把积分记录按该账号过滤——这是用户最初设想的入口
// （双击账号 → 该账号的积分变化历史）。
func (a *app) onAccountActivated() {
	idx := a.tvAccounts.CurrentIndex()
	r, ok := a.accounts.At(idx)
	if !ok {
		return
	}
	a.showCreditHistoryFor(r.UID)
}

// showCreditHistoryFor 切到日志页并只显示该账号的积分记录。
func (a *app) showCreditHistoryFor(uid string) {
	if uid == "" {
		return
	}
	a.setCreditFilter(uid)
	if a.tabs != nil {
		_ = a.tabs.SetCurrentIndex(a.logTabIndex())
	}
	log.Printf("查看账号 %s 的积分变化历史（共 %d 条）", a.displayName(uid), a.credits.RowCount())
}

// usageTabIndex 返回「用量」页的页签序号（按标题查找，避免插页后错位）。
func (a *app) usageTabIndex() int {
	return a.tabIndexByTitle("用量")
}

// tabIndexByTitle 按页签标题查找序号；找不到回落 0。
func (a *app) tabIndexByTitle(title string) int {
	if a.tabs == nil {
		return 0
	}
	for i := 0; i < a.tabs.Pages().Len(); i++ {
		if a.tabs.Pages().At(i).Title() == title {
			return i
		}
	}
	return 0
}

// logTabIndex 返回「日志」页的页签序号。
// 不再用硬编码常量：页签数量会随功能增减（如本次新增「测试」页），
// 硬编码会在插页后悄悄跳错页。按页标题查找，找不到回落 0（最差也回到首页）。
func (a *app) logTabIndex() int {
	return a.tabIndexByTitle("日志")
}

// At 返回账号表第 i 行（供双击等交互使用）。
func (m *accountModel) At(i int) (pool.Status, bool) {
	if i < 0 || i >= len(m.items) {
		return pool.Status{}, false
	}
	return m.items[i], true
}

// indexOfStr 返回 s 在 list 中的下标；不存在返回 -1。
func indexOfStr(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// equalStrs 报告两个字符串切片是否逐项相等。
func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// addCheckinRow 追加一条签到记录：UID 统一解析为昵称显示（与手动签到口径一致），
// 同时写入持久化存储（重启不丢）。
func (a *app) addCheckinRow(uid, result, detail string) {
	a.checkins.Prepend(checkinRow{
		At:     time.Now().Format("01-02 15:04:05"),
		UID:    a.displayName(uid),
		Result: result,
		Detail: detail,
	})
	if a.checkinLog != nil {
		a.checkinLog.Append(checkinRow{
			At:     time.Now().Format("01-02 15:04:05"),
			UID:    uid, // 落盘存 UID（昵称可能变），显示时再解析
			Result: result,
			Detail: detail,
		})
	}
}

// displayName 把 uid 解析成人眼可读的昵称；查不到时回落 uid 前 12 位。
func (a *app) displayName(uid string) string {
	if uid == "" {
		return "-"
	}
	for _, s := range a.svc.Accounts() {
		if s.UID == uid {
			if s.Nickname != "" {
				return s.Nickname
			}
			break
		}
	}
	if len(uid) > 12 {
		return uid[:12] + "…"
	}
	return uid
}

// loadCheckinHistory 启动时把持久化记录回填到表格（最新在前）。
func (a *app) loadCheckinHistory() {
	if a.checkinLog == nil {
		return
	}
	rows := a.checkinLog.Rows()
	if len(rows) == 0 {
		return
	}
	// Rows() 已是"最新在前"，逐条从尾部插入即可保持顺序稳定。
	items := make([]checkinRow, 0, len(rows))
	for _, r := range rows {
		r.UID = a.displayName(r.UID)
		items = append(items, r)
		if len(items) >= 300 {
			break
		}
	}
	a.checkins.Replace(items)
	log.Printf("已载入历史签到记录 %d 条", len(items))
}

// loadCreditHistory 把落盘的积分变动历史回填到表格（最新在前）。
// 存原始 UID，昵称由 creditModel.display 在渲染时解析。
func (a *app) loadCreditHistory() {
	if a.creditLog == nil {
		return
	}
	rows := a.creditLog.Rows()
	if len(rows) == 0 {
		return
	}
	// 不再按 500 截断：表格默认只看"全部"，但按账号过滤时要去重看完整历史，
	// 而落盘上限已提高到 20000（JSONL 只追加，代价与历史长度无关）。
	a.credits.Replace(rows)
	a.refreshCreditFilterLabel()
	log.Printf("已载入历史积分记录 %d 条", len(rows))
}

// catchUpCheckin 启动补签：检查每个账号今天是否已签到，未签到则补签一次。
//
// 设计约束（防限流）：
//   - 每账号每天最多补一次：先查持久化记录（TodayChecked），已签则跳过，绝不重复打上游；
//   - 串行 + 间隔，复用 credit 工具的 200ms 节奏；
//   - 失败不重试（记录失败即可），避免开机风暴。
func (a *app) catchUpCheckin() {
	if a.svc == nil || !a.svc.Running() {
		return
	}
	up := a.svc.Upstream()
	if up == nil {
		return
	}
	today := time.Now()
	var todo []pool.Status
	for _, st := range a.svc.Accounts() {
		if st.Disabled {
			continue
		}
		if a.checkinLog != nil && a.checkinLog.TodayChecked(st.UID, today) {
			continue
		}
		todo = append(todo, st)
	}
	if len(todo) == 0 {
		return
	}
	log.Printf("启动补签：%d 个账号今天未签到，开始补齐", len(todo))
	for _, st := range todo {
		at := a.svc.AuthByUID(st.UID)
		if at == nil {
			continue
		}
		result := "成功"
		detail := ""
		if err := up.DailyCheckin(at); err != nil {
			detail = err.Error()
			if upstream.IsAlreadyCheckedIn(detail) {
				result = "今天已签到"
			} else {
				result = "失败"
			}
		}
		a.addCheckinRow(st.UID, result, detail)
		log.Printf("启动补签 %s: %s", a.displayName(st.UID), result)
		// 签到成功顺带刷新额度（复用同一次上游窗口，不额外增加请求）
		if result == "成功" || result == "今天已签到" {
			if remain, err := up.UserResource(at); err == nil {
				a.svc.SetCreditsReason(st.UID, remain, "签到（启动补签）")
			}
		}
		time.Sleep(200 * time.Millisecond) // 防限流：串行 + 间隔
	}
}

// ── 额度刷新 ──

// doRefreshCredits 手动批量刷新所有账号积分显示。
// 串行 + 200ms 间隔（与定时刷新同口径），期间禁用按钮防重复点击。
func (a *app) doRefreshCredits() {
	if a.creditBusy {
		a.lblAccts2.SetText("额度刷新进行中，请稍候…")
		return
	}
	a.creditBusy = true
	if a.btnRefreshCredits != nil {
		a.btnRefreshCredits.SetEnabled(false)
	}
	go func() {
		defer func() {
			a.creditBusy = false
			a.mw.Synchronize(func() {
				if a.btnRefreshCredits != nil {
					a.btnRefreshCredits.SetEnabled(true)
				}
			})
		}()
		if !a.svc.Running() {
			a.mw.Synchronize(func() { a.lblAccts2.SetText("服务未运行，无法刷新额度") })
			return
		}
		sch := a.svc.Scheduler()
		if sch == nil {
			a.mw.Synchronize(func() { a.lblAccts2.SetText("调度器不可用") })
			return
		}
		items := a.svc.Accounts()
		a.mw.Synchronize(func() {
			a.lblAccts2.SetText(fmt.Sprintf("正在刷新 %d 个账号的额度…", len(items)))
		})
		n := sch.RunCreditRefreshNow()
		a.mw.Synchronize(func() {
			a.lblAccts2.SetText(fmt.Sprintf("额度刷新完成：成功 %d / 共 %d", n, len(items)))
			a.refreshAccounts()
		})
	}()
}

// loadModelRates 拉取模型参数（含思考深度/容量/能力/倍率）并填充表格。
// 失败静默，不影响其它功能。
func (a *app) loadModelRates() {
	if !a.svc.Running() {
		return
	}
	up := a.svc.Upstream()
	items := a.svc.Accounts()
	if up == nil || len(items) == 0 {
		return
	}
	// 用第一个可用账号拉模型表（模型表是所有账号共有的能力清单，非账号级数据）
	var acct = a.svc.AuthByUID(items[0].UID)
	if acct == nil {
		return
	}
	infos, err := up.FetchModels(acct)
	if err != nil {
		log.Printf("拉取模型参数失败: %v", err)
		return
	}
	rows := make([]modelRateRow, 0, len(infos))
	for _, mi := range infos {
		rate := "—"
		if mi.CreditsRate > 0 {
			rate = fmt.Sprintf("×%.2f", mi.CreditsRate)
		}
		// 思考档：上游对固定单档模型给 defaultEffort，对可调档模型给 supportedEfforts。
		effort := mi.DefaultEffort
		if effort == "" {
			effort = "—"
		}
		supported := "—"
		if len(mi.Efforts) > 0 {
			supported = strings.Join(mi.Efforts, "/")
		}
		canDisable := "—"
		if len(mi.Efforts) > 0 || mi.CanDisableThinking {
			canDisable = yesNo(mi.CanDisableThinking)
		}
		onlyReason := "—"
		if mi.OnlyReasoning {
			onlyReason = "是"
		}
		tags := "—"
		if len(mi.Tags) > 0 {
			tags = strings.Join(mi.Tags, ",")
		}
		isDef := "—"
		if mi.IsDefault {
			isDef = "是"
		}
		desc := mi.DescriptionZh
		if desc == "" {
			desc = mi.DescriptionEn
		}
		rows = append(rows, modelRateRow{
			ID:            mi.ID,
			Name:          mi.Name,
			Rate:          rate,
			RateV:         mi.CreditsRate,
			Effort:        effort,
			Supported:     supported,
			CanDisable:    canDisable,
			OnlyReason:    onlyReason,
			ContextWindow: mi.ContextWindow,
			MaxTokens:     mi.MaxTokens,
			Images:        yesNo(mi.SupportsImages),
			ToolCall:      yesNo(mi.SupportsToolCall),
			Reason:        yesNo(mi.SupportsReasoning),
			Vendor:        orDash(mi.Vendor),
			Tags:          tags,
			IsDeflt:       isDef,
			Desc:          desc,
		})
	}
	a.mw.Synchronize(func() {
		a.modelRates.Replace(rows)
		if a.lblModelHint != nil {
			a.lblModelHint.SetText(fmt.Sprintf("共 %d 个模型", len(rows)))
		}
		if a.lblModelDetail != nil && len(rows) > 0 {
			a.lblModelDetail.SetText("选中一行查看该模型的完整参数。")
		}
		a.syncProbeChoices()
		a.syncMatrixModels()
	})
	log.Printf("模型参数已加载：%d 个模型", len(rows))
}

// yesNo 布尔转中文显示。
func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

// orDash 空串显示为 "—"。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// estimateText 把「账号剩余积分 ÷ 模型倍率」换算成可用量估算。
func estimateText(credits int64, rate float64) string {
	if rate <= 0 {
		return "—"
	}
	return fmt.Sprintf("≈%.0f 次当量", float64(credits)/rate)
}

// ─────────────────────────── 应用 ───────────────────────────

type app struct {
	mw      *walk.MainWindow
	svc     *Service
	cfgPath string
	cfg     *appconfig.Config
	logs    *logBuffer
	icon    *walk.Icon

	accounts *accountModel
	checkins *checkinModel
	// checkinLog 签到记录持久化（重启不丢）。nil = 降级为纯内存。
	checkinLog *checkinStore
	// logRot gui.log 的轮转 writer（退出时关闭，释放 Windows 文件占用）。
	logRot *rotatingLogWriter
	// usageStore token 用量统计（账号×模型×日期）；退出时落盘。
	usageStore *server.TokenUsageStore
	// credits 积分变动记录表 + 持久化（重启不丢）。
	credits   *creditModel
	creditLog *creditStore
	// modelRates 模型消耗倍率表（×0.51 之类），与账号剩余积分共同换算可用量。
	modelRates *modelRateModel

	// 服务页
	lblState *walk.Label
	lblAddr  *walk.Label
	lblAccts *walk.Label
	// lblCopyHint 监听地址复制结果提示（独立于 lblState，避免被定时刷新冲掉）。
	lblCopyHint *walk.Label
	chkAuto     *walk.CheckBox

	// 账号页
	tvAccounts *walk.TableView
	lblAccts2  *walk.Label
	// lblTotalCredits 账号页的总积分统计（含未刷新额度的提示）。
	lblTotalCredits *walk.Label
	// btnRefreshCredits 手动批量刷新额度按钮（刷新期间禁用防重复点击）。
	btnRefreshCredits *walk.PushButton
	creditBusy        bool

	// 模型页
	tvModelRates   *walk.TableView
	lblModelHint   *walk.Label
	lblModelDetail *walk.Label

	// 登录页
	leURL     *walk.LineEdit
	lblLogin  *walk.Label
	oauth     *oauthflow.Client
	loginBusy bool

	// 用量页（token 统计：账号×模型 明细 + 账号级/全局汇总）
	tvUsage       *walk.TableView
	cbUsageScope  *walk.ComboBox
	cbUsageDay    *walk.ComboBox
	lblUsageTotal *walk.Label
	usage         *usageModel
	// usageDays 与 cbUsageDay 平行（索引 → "YYYY-MM-DD"；0 = 全部日期）。
	usageDays []string

	// 代理（账号级出口 IP）
	proxyReg          *proxy.Registry
	proxyCancel       context.CancelFunc
	proxyBindingsPath string
	proxyBindings     *proxyBindingModel
	tvProxyBindings   *walk.TableView
	lblProxyHint      *walk.Label
	// btnProxyToggle 一键开关代理；lblProxyState 显示当前开关状态。
	btnProxyToggle    *walk.PushButton
	lblProxyState     *walk.Label
	proxyBusy         bool
	teProxyCfg        *walk.Label // 生成的 listeners 配置（可复制）
	lastListenersYAML string

	// selectedAcctUID 用户当前选中的账号 UID。
	//
	// 为什么应用层要自己记：walk 只在 SetCurrentIndex 路径维护内部的
	// currentItemID（tableview.go:1217），而用户【鼠标点击】选行走的是
	// LVN_ITEMCHANGED，不更新它 → 重建时恢复循环匹配不到 → fallback 选中第 0 行。
	// 因此必须自己记录 UID，重建后按 UID 找回原行（行序可能因增删而变化）。
	selectedAcctUID string

	// lePriority 优先级输入框：每个账号可单独设权重值（选号权重乘子）。
	lePriority *walk.LineEdit

	// 账号页「检测模型可用性」：模型下拉框 + 检测进行中标志 + 取消标志 + 按钮。
	// cbMatrixModel 必须是账号页自己的控件——用户在点击检测前要能看到并修改模型。
	cbMatrixModel    *walk.ComboBox
	lastMatrixModels []string
	matrixBusy       bool
	matrixCancel     bool
	btnMatrixProbe   *walk.PushButton
	btnMatrixStop    *walk.PushButton

	// 测试页（手动探测账号×模型连通性）
	cbProbeAcct   *walk.ComboBox
	cbProbeModel  *walk.ComboBox
	leProbePrompt *walk.LineEdit // 自定义提示词（空 = 默认 probeDefaultPrompt「仅回复1」）
	leProbeTokens *walk.LineEdit // 自定义 max_tokens（空/非法 = 默认 32）
	btnProbe      *walk.PushButton
	teProbe       *walk.TextEdit
	probeBusy     bool
	// probeUIDs 与 cbProbeAcct 的选项平行（下拉框索引 → uid）。
	probeUIDs []string
	// lastProbeModels 上次填充模型下拉框的选项（判断是否需要重建）。
	lastProbeModels []string

	// logs page
	tabs      *walk.TabWidget
	tvCheckin *walk.TableView
	tvCredits *walk.TableView
	teLog     *walk.TextEdit
	// 积分记录过滤控件：下拉选账号 + “显示全部”。“
	// 双击「账号」页的行会把这里切到那个账号（见 onAccountActivated）。
	cbCreditFilter  *walk.ComboBox
	lblCreditFilter *walk.Label
	// lblAccountsHint 账号页底部提示标签（双击说明会在这里随选中状态变化）。
	lblAccountsHint *walk.Label
	// creditFilterUID 当前生效的积分记录过滤账号（creditAllUIDs = 全部）。
	creditFilterUID string
	// creditNames 下拉框选项（含 creditAllLabel 开头）与它们对应的 UID 平行数组。
	creditNames []string
	creditUIDs  []string

	// 配置页
	ed         map[string]*walk.LineEdit
	chkSticky  *walk.CheckBox
	lblCfgHint *walk.Label

	// 变更检测，避免每帧都重设控件内容（会打断用户选中/滚动）
	lastSig string
	lastLog string

	// 托盘与退出标志
	tray     *walk.NotifyIcon
	trayOK   bool // 托盘图标是否注册成功（决定关闭时能否用气泡提示）
	quitting bool
}

// logFilePath 落盘日志路径（启动后填入）；无控制台窗口时靠它排查问题。
var logFilePath string

// faultTolerantWriter 包一层：目标写失败时静默忽略，绝不阻断 MultiWriter 里
// 排在它后面的 writer。
//
// 背景（实测缺陷）：io.MultiWriter 遇错即短路返回。-H windowsgui 构建直接双击启动时
// 没有控制台，os.Stdout 是无效句柄、写必失败，而它原先排在 [内存, stdout, 文件] 的
// 中间——导致排在它后面的 gui.log 一次都收不到数据，文件日志自 04:01 启动后停更
// （文件停在 03:48），排障时完全没有落盘依据。
type faultTolerantWriter struct{ w io.Writer }

func (f faultTolerantWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		// 吞掉错误但如实报告已写字节数为 0，让 MultiWriter 继续走后面的 writer。
		return 0, nil
	}
	return n, nil
}

// setupLogging 让标准日志同时流向：内存日志面板、磁盘日志文件（带轮转）、控制台（若有）。
// 顺序刻意为 [内存, 文件, stdout]：
//   - 文件在 stdout 之前：stdout 失效（windowsgui 双击启动）不再殃及落盘；
//   - stdout 用容错包装：无控制台时静默跳过，有控制台时照常输出。
//
// 落盘位置按 exe 同目录 → %AppData% → 临时目录 依次尝试（环境里可能缺 APPDATA）。
func setupLogging(mem io.Writer) (io.Writer, *rotatingLogWriter) {
	// 先不把 os.Stdout 放进列表，等确定文件成功后再按 [内存, 文件, stdout] 顺序组装。
	var fileW *rotatingLogWriter

	var cands []string
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "gui.log"))
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		cands = append(cands, filepath.Join(dir, "WorkBuddy2API", "gui.log"))
	}
	cands = append(cands, filepath.Join(os.TempDir(), "WorkBuddy2API-gui.log"))

	for _, p := range cands {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			continue
		}
		// 用带轮转的 writer：按天切 + 单文件超 5MB 切，归档保留 14 天。
		// 打开失败时 rotate writer 自身会在下次 Write 重试，这里不需要额外兜底。
		rw := newRotatingLogWriter(p, guiLogMaxBytes, guiLogKeepDays*24*time.Hour)
		if err := rw.ensureForSetup(); err != nil {
			continue // 该候选路径完全不可用（目录建不了/打不开）→ 换下一个候选
		}
		fileW = rw
		logFilePath = p
		break
	}
	// 顺序：内存面板 → 磁盘文件 → 控制台（容错）。
	writers := []io.Writer{mem}
	if fileW != nil {
		writers = append(writers, fileW)
	}
	writers = append(writers, faultTolerantWriter{os.Stdout})
	return io.MultiWriter(writers...), fileW
}

func main() {
	log.SetFlags(log.Ltime)

	a := &app{
		svc:           NewService(),
		logs:          &logBuffer{},
		accounts:      &accountModel{},
		checkins:      &checkinModel{},
		credits:       &creditModel{},
		usage:         &usageModel{},
		proxyBindings: &proxyBindingModel{},
		modelRates:    &modelRateModel{},
		oauth:         oauthflow.NewClient(),
	}
	// 积分表的昵称解析：模型内只存原始 UID，渲染时才查昵称（改名不会影响过滤）。
	a.credits.display = a.displayName

	// 标准日志 → 内存面板 + 磁盘文件（带轮转）；请求日志也接到同一条链上
	sink, logRot := setupLogging(a.logs)
	a.logRot = logRot
	log.SetOutput(sink)
	server.SetChatLogWriter(sink)

	if err := a.loadConfig(); err != nil {
		log.Printf("读取配置失败: %v", err)
		walk.MsgBox(nil, appName, "读取配置失败：\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	// 签到记录持久化：与 state_file 同目录（data/），便于一起备份。
	a.checkinLog = newCheckinStore(filepath.Join(filepath.Dir(a.cfg.StateFile), checkinLogFile))
	// 账号级出口代理（每账号独立出口 IP）：读 Clash 节点 → 建分配表 → 恢复绑定。
	// 未启用/拉取失败时回落直连（不影响既有行为）。
	// 代理：优先用「一键开关」入口（GUI「代理」页）。
	// config.proxy.enabled=true 时在启动阶段自动开启（保持配置驱动的兼容性）；
	// 否则完全由用户点开关决定，无需改配置文件。
	a.proxyBindingsPath = a.cfg.Proxy.BindingsFile
	if a.cfg.Proxy.Enabled {
		go a.autoEnableProxyAtStartup()
	}

	// 积分变动历史持久化：同目录，重启不丢。
	a.creditLog = newCreditStore(filepath.Join(filepath.Dir(a.cfg.StateFile), creditLogFile))
	// token 用量统计（账号×模型×日期）：同目录。仅观测，不参与任何决策。
	a.usageStore = server.NewTokenUsageStore(filepath.Join(filepath.Dir(a.cfg.StateFile), tokenUsageFile))
	a.svc.SetUsageStore(a.usageStore)

	// 积分变动回调：pool 写路径触发（可能任意 goroutine）→ 表格刷新必须在 UI 线程，
	// 故用 Synchronize 投递；落盘在 creditStore 内部自行加锁，与 UI 线程解耦。
	// 注意：回调在 pool 持锁期间被调用，因此此处绝不能阻塞——Synchronize 是异步投递
	// （walk 的 Synchronize 只把函数排入 UI 队列并立即返回），不会与 pool 锁相互等待。
	a.svc.OnCreditsChanged = func(ch pool.CreditChange) {
		if a.creditLog != nil {
			a.creditLog.AppendChange(ch)
		}
		if a.mw == nil {
			return
		}
		// 回调里不做磁盘 IO（creditStore 自己落盘）；存原始 UID，渲染时才解析昵称。
		a.mw.Synchronize(func() {
			a.credits.Prepend(creditRow{
				At:     ch.At.Format("01-02 15:04:05"),
				UID:    ch.UID,
				Old:    ch.Old,
				New:    ch.New,
				Delta:  ch.Delta,
				First:  ch.First,
				Reason: ch.Reason,
			})
			a.refreshCreditFilterLabel()
		})
	}
	log.Printf("%s 启动 · 配置=%s · 监听=%s · 日志=%s", appName, a.cfgPath, a.cfg.Listen, logFilePath)

	a.svc.OnCheckin = func(uid string, status scheduler.CheckinStatus, detail string) {
		label := map[scheduler.CheckinStatus]string{
			scheduler.CheckinOK:      "成功",
			scheduler.CheckinAlready: "今天已签到",
			scheduler.CheckinFailed:  "失败",
		}[status]
		if label == "" {
			label = string(status)
		}
		a.addCheckinRow(uid, label, detail)
	}
	a.svc.OnAccountsChanged = func() { a.refreshAccounts() }
	// 额度刷新回调：只更新显示用积分（池内 SetCredits 已由调度器完成），
	// 失败仅记日志，不打断 UI。
	a.svc.OnCreditRefresh = func(uid string, remain int64, err error) {
		if err != nil {
			log.Printf("额度刷新 %s: %v", a.displayName(uid), err)
		}
	}

	if err := a.buildUI(); err != nil {
		log.Printf("界面初始化失败: %v", err)
		walk.MsgBox(nil, appName, "界面初始化失败：\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	log.Printf("界面初始化完成")

	// 自动启动服务（打不开端口时窗口里会显示原因）
	log.Printf("界面就绪，准备启动网关…")
	if err := a.svc.Start(a.cfg); err != nil {
		log.Printf("自动启动失败: %v", err)
	} else {
		log.Printf("网关已启动，监听 %s", a.cfg.Listen)
	}
	a.refreshAll()
	a.loadCheckinHistory()
	a.loadCreditHistory()
	a.refreshUsage()
	a.syncMatrixModels()
	a.refreshProxyBindings()
	a.refreshProxyToggle()

	go a.tickLoop()
	// 启动补签：在服务起来后异步执行，不阻塞 UI。
	// 仅对"今天尚无签到记录"的账号补签（每账号每天最多一次，防限流）。
	go a.catchUpCheckin()
	// 启动时拉一次模型倍率（供「模型」页展示）；失败静默。
	go a.loadModelRates()
	a.mw.Run()
}

// tickLoop 每 1.5 秒把状态与日志刷到界面上。
func (a *app) tickLoop() {
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if a.mw == nil {
			continue
		}
		a.mw.Synchronize(func() {
			a.refreshStatus()
			a.refreshAccounts()
			a.refreshLog()
			// 用量统计按需刷新：只在「用量」页可见时刷（其它页面刷了也看不见，
			// 白白重建表格）。周期与 tick 一致即可，token 统计不是实时指标。
			if a.tabs != nil && a.tabs.CurrentIndex() == a.usageTabIndex() {
				a.refreshUsage()
			}
		})
	}
}

func (a *app) refreshAll() {
	a.refreshStatus()
	a.refreshAccounts()
	a.refreshLog()
}

func (a *app) refreshStatus() {
	running := a.svc.Running()
	if running {
		uptime := shortDur(time.Since(a.svc.StartedAt()))
		a.lblState.SetText(fmt.Sprintf("运行中 · 已运行 %s", uptime))
		a.lblState.SetTextColor(walk.RGB(0x0F, 0x6E, 0x56))
	} else {
		msg := "已停止"
		if e := a.svc.LastErr(); e != "" {
			msg = "启动失败：" + firstLine(e)
		}
		a.lblState.SetText(msg)
		a.lblState.SetTextColor(walk.RGB(0xA3, 0x2D, 0x2D))
	}
	a.lblAddr.SetText(listenURLText(a.svc.ListenAddr(a.cfg)))

	items := a.svc.Accounts()
	healthy := 0
	for _, s := range items {
		if !s.Disabled && !s.Cooling && s.BreakerUntil.IsZero() {
			healthy++
		}
	}
	txt := fmt.Sprintf("账号 %d 个（可用 %d）", len(items), healthy)
	if running {
		if len(items) == 0 {
			txt += "  ·  还没有账号，去「登录」页添加"
		} else {
			txt += "  ·  Redis： " + a.svc.RedisMode()
		}
	}
	a.lblAccts.SetText(txt)
}

func (a *app) refreshAccounts() {
	items := a.svc.Accounts()
	// 总积分统计独立于表格签名比对：签名相同只意味着"不必重建表格"，
	// 不代表统计文案无需刷新（例如首次进入页面、或刷新额度后 credits 变化
	// 恰好被其他字段的相同值掩盖）。统计是纯字符串计算，代价可忽略。
	a.refreshTotalCredits(items)
	if a.tblSignature(items) == a.lastSig {
		return
	}
	a.accounts.Replace(items)
	if a.lblAccts2 != nil {
		a.lblAccts2.SetText(fmt.Sprintf("共 %d 个账号", len(items)))
	}
	// 重建后按 UID 恢复用户原来的选中行（Replace → PublishRowsReset 会清空选中，
	// 且 walk 内置的 currentItemID 机制对鼠标点击无效——见 selectedAcctUID 注释）。
	// 账号已被删除时 indexOfAccountUID 返回 -1，此时不恢复（避免误选中别的账号）。
	a.restoreAccountSelection(items)
	// 账号集可能变了（登录新增/删除/昵称更新）：同步积分过滤下拉框与测试页选择。
	// sync* 内部自行比较，选项未变时不会重置下拉框。
	a.syncCreditFilter()
	a.syncProbeChoices()
}

// tblSignature 生成"是否需要重建账号表格"的指纹。
//
// 只收录【变化慢、且需要重建表格才能反映】的字段：
// UID（账号增删）/ 昵称 / 积分 / 冷却 / 禁用 / 熔断计数 / 优先级。
//
// 刻意【排除】InFlight / InFlightPeak / PeakActive：
//   - 它们是高频运行态，每次请求都在变（实测请求间隔仅几秒），若进签名会让
//     签名几乎每个 tick（1.5s）都变化 → 反复 Replace → PublishRowsReset
//     重建表格 → 【用户选中的行被清空】（实测缺陷：选中几秒后自动取消）；
//   - 它们本来就不需要重建：表格 Value() 每帧实时读取当前值，签名不变时
//     单元格文字照样是最新的（walk 会重绘可见单元格）。
//
// 换言之：签名回答的是"表格结构/需要重置的内容变了吗"，不是"每个数字都变了吗"。
func (a *app) tblSignature(items []pool.Status) string {
	var b strings.Builder
	for _, s := range items {
		fmt.Fprintf(&b, "%s|%s|%d|%v|%v|%d|%g|%d|%v;",
			s.UID, s.Nickname, s.Credits, s.Cooling, s.Disabled, s.BreakerFails, s.Priority,
			s.ContentRejects, s.SuspectedBanned)
	}
	return b.String()
}

// shouldFollowTail 判定追加日志后是否自动滚到底：仅当视口此刻停在底部才跟随。
// 用户往上翻历史时视口不在底部 → 不跟随，不再被每 1.5s 强制拽回，
// 也避免 ScrollToCaret 把横向位置拉去长行末尾。
func shouldFollowTail(atBottom bool) bool {
	return atBottom
}

// logAtBottom 报告日志框垂直视口是否停在底部（容差 2px，规避像素级抖动）。
// 实现走 GetScrollInfo（walk 未封装），细节见 textedit_ex.go。
func (a *app) logAtBottom() bool {
	if a.teLog == nil {
		return true
	}
	return teVScrollAtBottom(a.teLog)
}

// refreshLog 把日志缓冲的新增行增量追加到界面日志框。
//
// 旧实现每 1.5s 全量 SetText + SetTextSelection(末尾) + ScrollToCaret，有三个副作用：
//  1. 全量重设让 EDIT 控件重建横向滚动范围（最长行宽度），滚动条忽宽忽窄；
//  2. ScrollToCaret 把光标滚到最后一行的【最右端】——最后一行是 6004 报文等长行时
//     视口被甩到最右，下一轮短行结尾又弹回最左，横向滚动条因此乱跳；
//  3. 用户手动往上翻历史时，视口每 1.5s 被强制拽回底部。
//
// 现实现：只追加新增行（控件保留既有滚动状态），且仅当用户本来就停在底部时
// 才垂直跟随到底（EM_SCROLL/SB_BOTTOM 只动垂直、不动水平）。
func (a *app) refreshLog() {
	if a.teLog == nil {
		return
	}
	newLines := a.logs.Drain()
	if len(newLines) == 0 {
		return
	}
	follow := shouldFollowTail(a.logAtBottom())
	// 追加（保留既有滚动位置），不再全量 SetText。
	var b strings.Builder
	for _, l := range newLines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	a.teLog.AppendText(b.String())
	if follow {
		teScrollToBottom(a.teLog)
	}
}

// ─────────────────────────── 配置读写 ───────────────────────────

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(p)
}

func (a *app) loadConfig() error {
	// 优先用 exe 同目录的 config.json，其次当前目录
	cands := []string{filepath.Join(exeDir(), cfgFile), cfgFile}
	a.cfgPath = cands[0]
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			a.cfgPath = c
			break
		}
	}
	cfg, err := appconfig.Load(a.cfgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		cfg, err = appconfig.Load("")
		if err != nil {
			return err
		}
	}
	a.cfg = cfg
	return nil
}

// rawConfig 以 map 形式读取配置文件，保留未知字段，避免保存时丢键。
func (a *app) rawConfig() (map[string]any, error) {
	raw, err := os.ReadFile(a.cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func getPath(m map[string]any, path ...string) string {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%g", v)
	case bool:
		return fmt.Sprintf("%v", v)
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func setPath(m map[string]any, val any, path ...string) {
	cur := m
	for i, k := range path {
		if i == len(path)-1 {
			cur[k] = val
			return
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
}

// ─────────────────────────── 操作 ───────────────────────────

func (a *app) doStart() {
	if err := a.svc.Start(a.cfg); err != nil {
		log.Printf("启动失败: %v", err)
	}
	a.refreshStatus()
}

func (a *app) doStop() {
	a.svc.Stop()
	a.refreshStatus()
}

func (a *app) doRestart() {
	if err := a.svc.Restart(a.cfg); err != nil {
		log.Printf("重启失败: %v", err)
	}
	a.refreshAll()
}

func (a *app) doSaveConfig(restart bool) {
	raw, err := a.rawConfig()
	if err != nil {
		a.cfgHint("保存失败：" + err.Error())
		return
	}
	setPath(raw, a.ed["listen"].Text(), "listen")
	setPath(raw, a.ed["api_key"].Text(), "api_key")
	setPath(raw, a.ed["auth_dir"].Text(), "auth_dir")
	setPath(raw, a.ed["state_file"].Text(), "state_file")
	setPath(raw, a.ed["soft_rate"].Text(), "cooldown", "soft_rate")
	setPath(raw, a.ed["max_rotate"].Text(), "upstream_rotate", "max_rotate")
	setPath(raw, a.ed["timeout"].Text(), "upstream", "timeout_seconds")
	setPath(raw, a.ed["header_timeout"].Text(), "upstream", "header_timeout_seconds")
	setPath(raw, a.ed["idle_timeout"].Text(), "upstream", "idle_timeout_seconds")
	setPath(raw, a.ed["checkin_hours"].Text(), "schedule", "checkin_hours")
	setPath(raw, a.ed["keepalive_hours"].Text(), "schedule", "keepalive_hours")
	setPath(raw, a.ed["credit_refresh"].Text(), "schedule", "credit_refresh")
	setPath(raw, a.ed["max_in_flight"].Text(), "pool", "max_in_flight")
	setPath(raw, a.ed["breaker_threshold"].Text(), "pool", "breaker_threshold")
	setPath(raw, a.ed["breaker_cooldown"].Text(), "pool", "breaker_cooldown")
	setPath(raw, a.ed["breaker_cooldown_max"].Text(), "pool", "breaker_cooldown_max")
	setPath(raw, a.chkSticky.Checked(), "session_sticky", "enabled")
	setPath(raw, a.ed["session_ttl"].Text(), "session_sticky", "ttl")
	setPath(raw, a.ed["session_gc"].Text(), "session_sticky", "gc_interval")

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		a.cfgHint("保存失败：" + err.Error())
		return
	}
	if err := os.WriteFile(a.cfgPath, out, 0o600); err != nil {
		a.cfgHint("写入失败：" + err.Error())
		return
	}

	cfg, err := appconfig.Load(a.cfgPath)
	if err != nil {
		a.cfgHint("已写入，但配置有误，请检查：" + err.Error())
		return
	}
	a.cfg = cfg
	if restart {
		if err := a.svc.Restart(cfg); err != nil {
			a.cfgHint("已保存，但重启失败：" + err.Error())
			return
		}
		a.cfgHint("已保存并重启：监听 " + cfg.Listen)
	} else {
		a.cfgHint("已保存（改端口/密钥后需重启才生效）")
	}
	a.refreshStatus()
}

func (a *app) cfgHint(s string) {
	if a.lblCfgHint != nil {
		a.lblCfgHint.SetText(s)
	}
	log.Printf("配置：%s", s)
}

// ── 登录 ──

func (a *app) doLoginStart() {
	if a.loginBusy {
		return
	}
	a.loginBusy = true
	a.lblLogin.SetText("正在向 CodeBuddy 申请授权链接…")
	a.mw.Synchronize(func() {})

	go func() {
		url, err := a.oauth.StartLogin()
		a.mw.Synchronize(func() {
			a.loginBusy = false
			if err != nil {
				a.lblLogin.SetText("申请失败：" + firstLine(err.Error()))
				return
			}
			a.leURL.SetText(url)
			a.lblLogin.SetText("已生成授权链接：点「打开浏览器」完成登录，然后回来点「我已完成登录」。")
		})
	}()
}

func (a *app) doOpenBrowser() {
	url := strings.TrimSpace(a.leURL.Text())
	if url == "" {
		a.lblLogin.SetText("请先点「生成授权链接」。")
		return
	}
	// 用 rundll32 调系统默认浏览器，避免走 cmd
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	a.lblLogin.SetText("已尝试在浏览器打开。若没反应，请手动复制上面的链接。")
}

// doCopyListenAddr 复制服务监听地址（带 http:// 前缀，可直接粘进浏览器/curl）。
// 复制内容取自状态栏当前显示的 URL，保证"看到的"与"复制的"完全一致。
func (a *app) doCopyListenAddr() {
	if a.lblAddr == nil {
		return
	}
	url := strings.TrimSpace(a.lblAddr.Text())
	if url == "" || url == "—" {
		return
	}
	if err := walk.Clipboard().SetText(url); err != nil {
		if a.lblCopyHint != nil {
			a.lblCopyHint.SetText("复制失败：" + firstLine(err.Error()))
		}
		return
	}
	if a.lblCopyHint != nil {
		a.lblCopyHint.SetText("已复制 ✓")
	}
}

func (a *app) doCopyURL() {
	url := strings.TrimSpace(a.leURL.Text())
	if url == "" {
		a.lblLogin.SetText("请先点「生成授权链接」。")
		return
	}
	if err := walk.Clipboard().SetText(url); err != nil {
		a.lblLogin.SetText("复制失败：" + err.Error())
		return
	}
	a.lblLogin.SetText("链接已复制到剪贴板。")
}

// doLoginDone 轮询登录结果；成功后落盘并热加载，无需重启服务。
func (a *app) doLoginDone() {
	if a.loginBusy {
		return
	}
	a.loginBusy = true
	a.lblLogin.SetText("正在检查登录结果…")

	go func() {
		defer func() { a.loginBusy = false }()
		b, err := a.oauth.PollLogin()
		if err != nil {
			a.mw.Synchronize(func() {
				if err == oauthflow.ErrPending || strings.Contains(err.Error(), "pending") {
					a.lblLogin.SetText("还没检测到登录完成。请在浏览器里把登录走完，再点一次。")
				} else {
					a.lblLogin.SetText("获取失败：" + firstLine(err.Error()))
				}
			})
			return
		}
		path, overwritten, err := oauthflow.SaveAuthFile(a.cfg.AuthDir, b)
		if err != nil {
			a.mw.Synchronize(func() { a.lblLogin.SetText("保存凭证失败：" + firstLine(err.Error())) })
			return
		}
		// 热加载进账号池
		n, rerr := a.svc.ReloadAccounts()
		a.mw.Synchronize(func() {
			verb := "新增"
			if overwritten {
				verb = "覆盖更新"
			}
			msg := fmt.Sprintf("登录成功！已%s账号 %s（%s）。", verb, stringOr(b.Nickname, b.UID), filepath.Base(path))
			if rerr != nil {
				msg += " 服务未运行，重启后生效。"
			} else {
				msg += fmt.Sprintf(" 已热加载，当前账号 %d 个。", n)
			}
			a.lblLogin.SetText(msg)
			a.leURL.SetText("")
			a.refreshAccounts()
		})
	}()
}

// ── 账号操作 ──

func (a *app) doCheckinAll() {
	go func() {
		items := a.svc.Accounts()
		if len(items) == 0 {
			a.mw.Synchronize(func() { a.lblAccts2.SetText("没有账号可签到") })
			return
		}
		a.mw.Synchronize(func() { a.lblAccts2.SetText(fmt.Sprintf("正在为 %d 个账号签到…", len(items))) })

		up := a.svc.Upstream()
		if up == nil {
			a.mw.Synchronize(func() { a.lblAccts2.SetText("服务未运行，无法签到") })
			return
		}
		ok, already, fail := 0, 0, 0
		for _, st := range items {
			at := a.svc.AuthByUID(st.UID)
			if at == nil {
				continue
			}
			detail := ""
			result := "成功"
			if err := up.DailyCheckin(at); err != nil {
				detail = err.Error()
				if upstream.IsAlreadyCheckedIn(detail) {
					result = "今天已签到"
					already++
				} else {
					result = "失败"
					fail++
				}
			} else {
				ok++
			}
			a.addCheckinRow(st.UID, result, detail)
		}
		a.mw.Synchronize(func() {
			a.lblAccts2.SetText(fmt.Sprintf("签到完成：成功 %d · 已签到 %d · 失败 %d", ok, already, fail))
			a.refreshAccounts()
		})
	}()
}

// refreshTotalCredits 更新账号页的总积分统计（纯展示，不发任何上游请求）。
// 放在 refreshAccounts 内：账号集/积分变化都会经过它。
func (a *app) refreshTotalCredits(items []pool.Status) {
	if a.lblTotalCredits == nil {
		return
	}
	a.lblTotalCredits.SetText(totalCreditsText(items))
}

// doClearSuspectBan 手动解除选中账号的"疑似被上游拉黑"判定。
//
// 用途：用户确认该账号已恢复时立刻给它一次机会，不必等 1 小时冷却到期。
// 不消耗积分——只改本地状态，不发任何复检请求。
func (a *app) doClearSuspectBan() {
	items := a.svc.Accounts()
	a.takeSelection(func(idx int) {
		if idx < 0 || idx >= len(items) {
			a.lblAccts2.SetText("请先在表格里选中一个账号")
			return
		}
		st := items[idx]
		a.svc.ClearSuspectBan(st.UID)
		a.refreshAccountsNow()
		a.lblAccts2.SetText(fmt.Sprintf("%s：已解除疑似拉黑判定（计数与冷却已清零）", a.displayName(st.UID)))
		log.Printf("手动解除疑似拉黑: %s（原连续拒审 %d 次）", a.displayName(st.UID), st.ContentRejects)
	})
}

// doSetPriority 把选中账号设为目标优先级（0 = 取消）。
//
// 用途：标记"一次性登录"账号（手机号一次性、失效后无法二次登录），
// 让它们的额度被优先消耗掉，避免浪费。
// 语义是【权重乘子】而非绝对优先：高优先级账号被选中的概率显著更大，
// 但仍保留随机性，因此限流/在途满时会自动让位给其他账号
// （集中打一个账号会更快撞 6004，反而更慢用完）。
func (a *app) doSetPriority(priority float64) {
	items := a.svc.Accounts()
	a.takeSelection(func(idx int) {
		if idx < 0 || idx >= len(items) {
			a.lblAccts2.SetText("请先在表格里选中一个账号")
			return
		}
		st := items[idx]
		a.svc.SetPriority(st.UID, priority)
		// 立即回填显示（refreshAccounts 有签名比对，标记变化会让签名变化）
		a.refreshAccountsNow()
		if priority > 0 {
			a.lblAccts2.SetText(fmt.Sprintf("%s 已设为优先消耗（×%g）", a.displayName(st.UID), priority))
		} else {
			a.lblAccts2.SetText(fmt.Sprintf("%s 已取消优先消耗", a.displayName(st.UID)))
		}
	})
}

// doApplyPriority 把输入框里的权重值应用到选中账号。
//
// 权重值是选号的【权重乘子】：>1 更容易被选中（一次性账号希望优先消耗额度时用），
// <1 降低优先级，0 或留空 = 取消设置（回到 1.0，行为与未标记一致）。
func (a *app) doApplyPriority() {
	raw := ""
	if a.lePriority != nil {
		raw = strings.TrimSpace(a.lePriority.Text())
	}
	if raw == "" {
		a.lblAccts2.SetText("请先在「优先级」框里填写权重值（如 3 或 0.5）")
		return
	}
	v, ok := parsePriorityInput(raw)
	if !ok {
		a.lblAccts2.SetText("权重值无效（需为非负数字，如 3 / 0.5 / 0）：" + raw)
		return
	}
	a.doSetPriority(v)
}

// parsePriorityInput 解析优先级输入：接受非负浮点数（含科学计数法）；
// 空串或非法输入返回 ok=false（调用方给提示，不静默当 0 处理）。
func parsePriorityInput(raw string) (float64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// restoreAccountSelection 表格重建后按 UID 恢复选中行。
// 必须在 accounts.Replace 之后调用（Replace 会清空选中）。
func (a *app) restoreAccountSelection(items []pool.Status) {
	if a.tvAccounts == nil || a.selectedAcctUID == "" {
		return
	}
	idx := indexOfAccountUID(items, a.selectedAcctUID)
	if idx < 0 {
		// 账号已被删除：清掉记录，避免下次误恢复。
		a.selectedAcctUID = ""
		return
	}
	if a.tvAccounts.CurrentIndex() != idx {
		_ = a.tvAccounts.SetCurrentIndex(idx)
	}
}

// rememberAccountSelection 记录当前选中行的 UID（表格选中变化时调用）。
func (a *app) rememberAccountSelection() {
	if a.tvAccounts == nil {
		return
	}
	idx := a.tvAccounts.CurrentIndex()
	if r, ok := a.accounts.At(idx); ok {
		a.selectedAcctUID = r.UID
	}
}

// refreshAccountsNow 强制刷新账号表（绕过签名比对），用于标记等需要立即生效的场景。
func (a *app) refreshAccountsNow() {
	a.lastSig = ""
	a.refreshAccounts()
}

func (a *app) doDeleteAccount() {
	items := a.svc.Accounts()
	a.takeSelection(func(idx int) {
		if idx < 0 || idx >= len(items) {
			a.lblAccts2.SetText("请先在表格里选中一个账号")
			return
		}
		st := items[idx]
		if walk.MsgBox(a.mw, appName,
			"确定删除账号 "+stringOr(st.Nickname, st.UID)+" 吗？\n\n会删除 auths 下的凭证文件（不可恢复）。",
			walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
			return
		}
		path := filepath.Join(a.cfg.AuthDir, "workbuddy-"+st.UID+".json")
		if err := os.Remove(path); err != nil {
			a.lblAccts2.SetText("删除失败：" + err.Error())
			return
		}
		n, err := a.svc.ReloadAccounts()
		if err != nil {
			a.lblAccts2.SetText("已删除文件；服务未运行，重启后生效。")
		} else {
			a.lblAccts2.SetText(fmt.Sprintf("已删除，剩余 %d 个账号", n))
		}
		a.refreshAccounts()
	})
}

// takeSelection 取表格当前选中行（walk 的消息循环要求回到 UI 线程处理）。
func (a *app) takeSelection(fn func(int)) {
	idx := a.tvAccounts.CurrentIndex()
	fn(idx)
}

// ── 开机自启 ──

func autoStartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runKey)
	return err == nil && v != ""
}

func setAutoStart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		err := k.DeleteValue(runKey)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runKey, `"`+exe+`"`)
}

// ─────────────────────────── 小工具 ───────────────────────────

func stringOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func splitInts(s string) []int {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '[' || r == ']'
	})
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(f, "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func atoiOr(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return def
	}
	return n
}

// makeAppIcon 内存里画一个图标，免去外置 .ico 文件。
func makeAppIcon() image.Image {
	const s = 32
	img := image.NewRGBA(image.Rect(0, 0, s, s))
	fg := color.RGBA{0x1D, 0x9E, 0x75, 0xFF}
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			dx, dy := x-s/2, y-s/2
			if dx*dx+dy*dy <= (s/2-2)*(s/2-2) {
				img.Set(x, y, fg)
			}
		}
	}
	white := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	for y := 14; y < 18; y++ {
		for x := 8; x < 24; x++ {
			img.Set(x, y, white)
		}
	}
	for y := 10; y < 22; y++ {
		for x := 14; x < 18; x++ {
			img.Set(x, y, white)
		}
	}
	return img
}
