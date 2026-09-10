// WorkBuddy2API 桌面控制台（Windows / Go + walk）。
//
// 设计要点：网关服务与界面跑在同一个进程里，所以能随时启停、加完账号立即热加载，
// 不需要再单独拉起 wb2api.exe。
package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows/registry"

	"workbuddy2api/internal/appconfig"
	"workbuddy2api/internal/oauthflow"
	"workbuddy2api/internal/pool"
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
		b.lines = append(b.lines, b.part[:i])
		b.part = b.part[i+1:]
	}
	if len(b.lines) > logLines {
		b.lines = b.lines[len(b.lines)-logLines:]
	}
	return len(p), nil
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

// modelRateRow 模型倍率表一行。
type modelRateRow struct {
	ID    string
	Name  string
	Rate  string
	RateV float64
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
		return r.Rate
	default:
		return ""
	}
}

func (m *modelRateModel) Replace(items []modelRateRow) {
	m.items = items
	m.PublishRowsReset()
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
	case 5:
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

// accountState 把账号状态压成一个人眼可读的短标签。
func accountState(s pool.Status) string {
	switch {
	case s.Disabled:
		return "已禁用（需重新登录）"
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

type checkinModel struct {
	walk.TableModelBase
	items []checkinRow
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
				a.svc.SetCredits(st.UID, remain)
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

// loadModelRates 拉取模型消耗倍率并填充表格（失败静默，不影响其它功能）。
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
		log.Printf("拉取模型倍率失败: %v", err)
		return
	}
	rows := make([]modelRateRow, 0, len(infos))
	for _, mi := range infos {
		rate := "—"
		if mi.CreditsRate > 0 {
			rate = fmt.Sprintf("×%.2f", mi.CreditsRate)
		}
		rows = append(rows, modelRateRow{
			ID:    mi.ID,
			Name:  mi.Name,
			Rate:  rate,
			RateV: mi.CreditsRate,
		})
	}
	a.mw.Synchronize(func() { a.modelRates.Replace(rows) })
	log.Printf("模型倍率已加载：%d 个模型", len(rows))
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
	// modelRates 模型消耗倍率表（×0.51 之类），与账号剩余积分共同换算可用量。
	modelRates *modelRateModel

	// 服务页
	lblState *walk.Label
	lblAddr  *walk.Label
	lblAccts *walk.Label
	chkAuto  *walk.CheckBox

	// 账号页
	tvAccounts *walk.TableView
	lblAccts2  *walk.Label
	// btnRefreshCredits 手动批量刷新额度按钮（刷新期间禁用防重复点击）。
	btnRefreshCredits *walk.PushButton
	creditBusy        bool

	// 模型页
	tvModelRates *walk.TableView

	// 登录页
	leURL     *walk.LineEdit
	lblLogin  *walk.Label
	oauth     *oauthflow.Client
	loginBusy bool

	// 日志页
	tvCheckin *walk.TableView
	teLog     *walk.TextEdit

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

// setupLogging 让标准日志同时流向：内存日志面板、磁盘日志文件。
// 落盘位置按 exe 同目录 → %AppData% → 临时目录 依次尝试（环境里可能缺 APPDATA）。
func setupLogging(mem io.Writer) io.Writer {
	writers := []io.Writer{mem, os.Stdout}

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
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			continue
		}
		writers = append(writers, f)
		logFilePath = p
		break
	}
	return io.MultiWriter(writers...)
}

func main() {
	log.SetFlags(log.Ltime)

	a := &app{
		svc:        NewService(),
		logs:       &logBuffer{},
		accounts:   &accountModel{},
		checkins:   &checkinModel{},
		modelRates: &modelRateModel{},
		oauth:      oauthflow.NewClient(),
	}

	// 标准日志 → 内存面板 + 磁盘文件；请求日志也接到同一条链上
	sink := setupLogging(a.logs)
	log.SetOutput(sink)
	server.SetChatLogWriter(sink)

	if err := a.loadConfig(); err != nil {
		log.Printf("读取配置失败: %v", err)
		walk.MsgBox(nil, appName, "读取配置失败：\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	// 签到记录持久化：与 state_file 同目录（data/），便于一起备份。
	a.checkinLog = newCheckinStore(filepath.Join(filepath.Dir(a.cfg.StateFile), checkinLogFile))
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
	a.lblAddr.SetText(a.svc.ListenAddr(a.cfg))

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
	if a.tblSignature(items) == a.lastSig {
		return
	}
	a.accounts.Replace(items)
	if a.lblAccts2 != nil {
		a.lblAccts2.SetText(fmt.Sprintf("共 %d 个账号", len(items)))
	}
}

func (a *app) tblSignature(items []pool.Status) string {
	var b strings.Builder
	for _, s := range items {
		fmt.Fprintf(&b, "%s|%d|%v|%v|%d|%v|%d|%d;", s.UID, s.Credits, s.Cooling, s.Disabled, s.InFlight, s.PeakActive, s.InFlightPeak, s.BreakerFails)
	}
	return b.String()
}

func (a *app) refreshLog() {
	if a.teLog == nil {
		return
	}
	txt := a.logs.Text()
	if txt == a.lastLog {
		return
	}
	a.lastLog = txt
	a.teLog.SetText(txt)
	a.teLog.SetTextSelection(len(txt), len(txt))
	a.teLog.ScrollToCaret()
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
