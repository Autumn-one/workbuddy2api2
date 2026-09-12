package main

import (
	"fmt"
	"time"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// buildUI 装配主窗口（5 个页签）+ 系统托盘。
func (a *app) buildUI() error {
	ic, err := walk.NewIconFromImage(makeAppIcon())
	if err != nil {
		return err
	}
	a.icon = ic

	raw, _ := a.rawConfig()

	// ── 配置页控件句柄 ──
	var leListen, leAPIKey, leAuthDir, leStateFile *walk.LineEdit
	var leSoftRate, leTimeout, leHeaderTO, leIdleTO *walk.LineEdit
	var leCheckinHours, leKeepaliveHours, leCreditRefresh *walk.LineEdit
	var leMaxInFlight, leBreakerTh, leBreakerCd, leBreakerCdMax *walk.LineEdit
	var leSessionTTL, leSessionGC *walk.LineEdit

	// ── 账号页/日志页 ──
	var btnRefresh, btnCheckin, btnDelete *walk.PushButton
	var btnReloadRates *walk.PushButton
	var btnLoginStart, btnCopy, btnOpen, btnDone *walk.PushButton

	mw := dcl.MainWindow{
		AssignTo: &a.mw,
		Title:    appName + " · 控制台",
		Icon:     ic,
		MinSize:  dcl.Size{Width: 880, Height: 600},
		Size:     dcl.Size{Width: 1080, Height: 720},
		Font:     dcl.Font{Family: "Segoe UI", PointSize: 9},
		Layout:   dcl.VBox{MarginsZero: false},
		Children: []dcl.Widget{
			dcl.TabWidget{
				AssignTo: &a.tabs,
				Pages: []dcl.TabPage{
					// ══════════════ 服务 ══════════════
					{
						Title:  "服务",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 12},
						Children: []dcl.Widget{
							dcl.GroupBox{
								Title:  "运行状态",
								Layout: dcl.Grid{Columns: 2, Spacing: 10},
								Children: []dcl.Widget{
									dcl.Label{Text: "状态"},
									dcl.Label{AssignTo: &a.lblState, Text: "未启动", Font: dcl.Font{Family: "Segoe UI", PointSize: 10, Bold: true}},
									dcl.Label{Text: "监听地址"},
									dcl.Composite{
										Layout: dcl.HBox{Spacing: 6},
										Children: []dcl.Widget{
											dcl.Label{AssignTo: &a.lblAddr, Text: "—"},
											dcl.PushButton{
												Text:      "复制",
												MinSize:   dcl.Size{Width: 52},
												OnClicked: a.doCopyListenAddr,
											},
											// 复制结果提示：刻意不用 lblState —— 它每 1.5s 被
											// refreshStatus 重写，提示会被立刻冲掉。
											dcl.Label{AssignTo: &a.lblCopyHint, Text: ""},
											dcl.HSpacer{},
										},
									},
									dcl.Label{Text: "账号"},
									dcl.Label{AssignTo: &a.lblAccts, Text: "—"},
								},
							},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.PushButton{Text: "启动服务", MinSize: dcl.Size{Width: 96}, OnClicked: a.doStart},
									dcl.PushButton{Text: "停止服务", MinSize: dcl.Size{Width: 96}, OnClicked: a.doStop},
									dcl.PushButton{Text: "重启服务", MinSize: dcl.Size{Width: 96}, OnClicked: a.doRestart},
									dcl.HSpacer{},
									dcl.PushButton{Text: "打开日志", MinSize: dcl.Size{Width: 96}, OnClicked: func() {
										if a.mw != nil {
											_ = a.mw.SetFocus()
										}
									}},
								},
							},
							dcl.GroupBox{
								Title:  "启动选项",
								Layout: dcl.VBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.CheckBox{
										AssignTo: &a.chkAuto,
										Text:     "开机自动启动（随 Windows 登录启动本控制台，并自动拉起网关）",
										OnClicked: func() {
											if err := setAutoStart(a.chkAuto.Checked()); err != nil {
												walk.MsgBox(a.mw, appName, "设置开机自启失败：\n"+err.Error(), walk.MsgBoxIconError)
												a.chkAuto.SetChecked(autoStartEnabled())
											}
										},
									},
								},
							},
							dcl.VSpacer{},
						},
					},

					// ══════════════ 账号 ══════════════
					{
						Title:  "账号",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 10},
						Children: []dcl.Widget{
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.PushButton{AssignTo: &btnRefresh, Text: "刷新", MinSize: dcl.Size{Width: 80}, OnClicked: a.refreshAccounts},
									dcl.PushButton{AssignTo: &a.btnRefreshCredits, Text: "刷新额度", MinSize: dcl.Size{Width: 90}, OnClicked: a.doRefreshCredits},
									// 单账号操作：针对选中的那一行（批量版是"全部"，两者并存）。
									dcl.PushButton{Text: "签到选中", MinSize: dcl.Size{Width: 80}, OnClicked: a.doCheckinSelected},
									dcl.PushButton{Text: "刷新选中额度", MinSize: dcl.Size{Width: 100}, OnClicked: a.doRefreshCreditsSelected},
									dcl.PushButton{AssignTo: &btnCheckin, Text: "手动签到全部", MinSize: dcl.Size{Width: 110}, OnClicked: a.doCheckinAll},
									// 优先级：每个账号可单独设权重值（选号权重乘子）。
									// 权重越大越常被选中（如一次性账号希望优先消耗其额度）；
									// <1 表示降低优先级；0 = 取消设置（回到 1.0）。
									dcl.Label{Text: "优先级"},
									dcl.LineEdit{AssignTo: &a.lePriority, CueBanner: "如 3 / 0.5", MinSize: dcl.Size{Width: 70}},
									dcl.PushButton{Text: "设置", MinSize: dcl.Size{Width: 60}, OnClicked: a.doApplyPriority},
									dcl.PushButton{Text: "取消", MinSize: dcl.Size{Width: 60}, OnClicked: func() { a.doSetPriority(0) }},
									dcl.PushButton{AssignTo: &btnDelete, Text: "删除选中账号", MinSize: dcl.Size{Width: 110}, OnClicked: a.doDeleteAccount},
									// 一键检测：先选模型，再横向扫全部账号的可用性。
									// 模型下拉框必须在账号页——用户点击前要能看到并修改要测哪个模型。
									// 会消耗少量积分（每账号 1 次小请求），点击后有确认提示。
									dcl.Label{Text: "检测模型"},
									dcl.ComboBox{AssignTo: &a.cbMatrixModel, Model: []string{"（请先到「模型」页加载参数）"}, CurrentIndex: 0, MinSize: dcl.Size{Width: 180}},
									dcl.PushButton{AssignTo: &a.btnMatrixProbe, Text: "检测模型可用性", MinSize: dcl.Size{Width: 120}, OnClicked: a.doMatrixProbe},
									dcl.PushButton{AssignTo: &a.btnMatrixStop, Text: "停止检测", MinSize: dcl.Size{Width: 80}, OnClicked: a.doStopMatrixProbe},
									dcl.HSpacer{},
									dcl.Label{AssignTo: &a.lblAccts2, Text: ""},
								},
							},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									// 总积分统计：账号页一眼看到全部账号的积分总量。
									// 字体加大加粗以便扫视；未知账号（未刷新过额度）不计入并明确提示。
									dcl.Label{
										AssignTo: &a.lblTotalCredits,
										Text:     "总积分 0 · 0 个账号",
										Font:     dcl.Font{Family: "Segoe UI", PointSize: 11, Bold: true},
									},
									dcl.HSpacer{},
								},
							},
							dcl.TableView{
								AssignTo:         &a.tvAccounts,
								Model:            a.accounts,
								AlternatingRowBG: true,
								StretchFactor:    1,
								// 双击一行 = 直接看该账号的积分变化历史（跳到「日志」页并过滤）。
								OnItemActivated: a.onAccountActivated,
								Columns: []dcl.TableViewColumn{
									{Title: "昵称", Width: 150},
									{Title: "UID", Width: 150},
									{Title: "积分", Width: 80, Alignment: dcl.AlignFar},
									{Title: "状态", Width: 190},
									{Title: "优先级", Width: 70, Alignment: dcl.AlignFar},
									{Title: "在途", Width: 60, Alignment: dcl.AlignFar},
									{Title: "成功/总", Width: 90, Alignment: dcl.AlignFar},
								},
							},
							dcl.Label{AssignTo: &a.lblAccountsHint, Text: "提示：账号来自 auths 目录下的凭证文件。选中一行后可「签到选中」「刷新选中额度」或删除；双击一行看该账号的积分变化历史。"},
						},
					},

					// ══════════════ 模型 ══════════════
					{
						Title:  "模型",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 10},
						Children: []dcl.Widget{
							dcl.Label{Text: "模型参数（全部来自上游 models 接口实时拉取，非本地写死；缓存 1 小时）。" +
								"\r\n· 思考深度：默认档（上游 defaultEffort）+ 可选档（supportedEfforts）。可调档模型才能切档，" +
								"其余为固定单档。请求里写 reasoning_effort 时，网关会按可选档自动降级。" +
								"\r\n· 倍率越低越省积分：账号剩余积分 ÷ 倍率 ≈ 可用次数当量。" +
								"\r\n· 注意：上游按【账号统一积分池】扣费，不存在模型级额度；倍率仅反映消耗速率差异。" +
								"\r\n提示：表格列较多，可拖动表头边界或横向滚动查看全部参数。"},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.PushButton{AssignTo: &btnReloadRates, Text: "重新加载参数", MinSize: dcl.Size{Width: 110}, OnClicked: func() { go a.loadModelRates() }},
									dcl.HSpacer{},
									dcl.Label{AssignTo: &a.lblModelHint, Text: ""},
								},
							},
							dcl.TableView{
								AssignTo:         &a.tvModelRates,
								Model:            a.modelRates,
								AlternatingRowBG: true,
								StretchFactor:    1,
								Columns: []dcl.TableViewColumn{
									{Title: "模型 ID", Width: 160},
									{Title: "名称", Width: 150},
									{Title: "默认思考", Width: 80},
									{Title: "可选思考档", Width: 110},
									{Title: "可关思考", Width: 80},
									{Title: "倍率", Width: 70, Alignment: dcl.AlignFar},
									{Title: "上下文", Width: 90, Alignment: dcl.AlignFar},
									{Title: "最大输出", Width: 90, Alignment: dcl.AlignFar},
									{Title: "图片", Width: 55},
									{Title: "工具", Width: 55},
									{Title: "厂商", Width: 55},
									{Title: "说明", Width: 200},
								},
							},
							dcl.Label{AssignTo: &a.lblModelDetail, Text: "选中一行查看该模型的完整参数。"},
						},
					},

					// ══════════════ 测试 ══════════════
					{
						Title:  "测试",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 10},
						Children: []dcl.Widget{
							dcl.Label{Text: "手动探测：选定账号 + 模型，用该账号凭证直接打一次上游（绕过网关轮换）。" +
								"\r\n用途：判断某账号不可用是账号问题还是模型问题；探测请求固定小 max_tokens，积分消耗极小。"},
							dcl.Composite{
								Layout: dcl.Grid{Columns: 2, Spacing: 8},
								Children: []dcl.Widget{
									dcl.Label{Text: "账号"},
									dcl.Label{Text: "模型"},
									dcl.ComboBox{AssignTo: &a.cbProbeAcct, MinSize: dcl.Size{Width: 260}},
									dcl.ComboBox{AssignTo: &a.cbProbeModel, MinSize: dcl.Size{Width: 260}},
									dcl.Label{Text: "提示词（可选）"},
									dcl.Label{Text: "输出上限（可选）"},
									dcl.LineEdit{AssignTo: &a.leProbePrompt, CueBanner: "留空 = 仅回复1", MinSize: dcl.Size{Width: 260}},
									dcl.LineEdit{AssignTo: &a.leProbeTokens, CueBanner: "留空 = 32", MinSize: dcl.Size{Width: 260}},
								},
							},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.PushButton{AssignTo: &a.btnProbe, Text: "开始测试", MinSize: dcl.Size{Width: 100}, OnClicked: a.doProbe},
									dcl.HSpacer{},
								},
							},
							dcl.GroupBox{
								Title:  "结果",
								Layout: dcl.VBox{},
								Children: []dcl.Widget{
									dcl.TextEdit{AssignTo: &a.teProbe, ReadOnly: true, VScroll: true, StretchFactor: 1,
										Font: dcl.Font{Family: "Cascadia Mono", PointSize: 9}},
								},
							},
						},
					},

					// ══════════════ 登录 ══════════════
					{
						Title:  "登录",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 10},
						Children: []dcl.Widget{
							dcl.Label{Text: "添加 CodeBuddy 账号：先生成授权链接，在浏览器里完成登录，再回来确认。" +
								"\r\n成功后会自动保存凭证并热加载进账号池，不需要重启服务。"},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.PushButton{AssignTo: &btnLoginStart, Text: "① 生成授权链接", MinSize: dcl.Size{Width: 140}, OnClicked: a.doLoginStart},
									dcl.HSpacer{},
								},
							},
							dcl.Label{Text: "授权链接"},
							dcl.LineEdit{AssignTo: &a.leURL, ReadOnly: true, Text: ""},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.PushButton{AssignTo: &btnOpen, Text: "② 打开浏览器", MinSize: dcl.Size{Width: 110}, OnClicked: a.doOpenBrowser},
									dcl.PushButton{AssignTo: &btnCopy, Text: "复制链接", MinSize: dcl.Size{Width: 90}, OnClicked: a.doCopyURL},
									dcl.PushButton{AssignTo: &btnDone, Text: "③ 我已完成登录", MinSize: dcl.Size{Width: 130}, OnClicked: a.doLoginDone},
									dcl.HSpacer{},
								},
							},
							dcl.Label{AssignTo: &a.lblLogin, Text: "等待操作。"},
							dcl.VSpacer{},
						},
					},

					// ══════════════ 日志 ══════════════
					{
						Title:  "日志",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 10},
						Children: []dcl.Widget{
							dcl.GroupBox{
								Title:         "签到记录（自动签到 与 手动签到都在这里）",
								Layout:        dcl.VBox{},
								StretchFactor: 1,
								Children: []dcl.Widget{
									dcl.TableView{
										AssignTo:         &a.tvCheckin,
										Model:            a.checkins,
										AlternatingRowBG: true,
										StretchFactor:    1,
										Columns: []dcl.TableViewColumn{
											{Title: "时间", Width: 120},
											{Title: "账号", Width: 190},
											{Title: "结果", Width: 100},
											{Title: "详情", Width: 420},
										},
									},
								},
							},
							dcl.GroupBox{
								Title:         "积分记录（每次积分变动都在这里：签到 +100、调用消耗、额度刷新）",
								Layout:        dcl.VBox{},
								StretchFactor: 1,
								Children: []dcl.Widget{
									dcl.Composite{
										Layout: dcl.HBox{Spacing: 8},
										Children: []dcl.Widget{
											dcl.Label{Text: "按账号过滤"},
											dcl.ComboBox{
												AssignTo:              &a.cbCreditFilter,
												Model:                 []string{creditAllLabel},
												CurrentIndex:          0,
												OnCurrentIndexChanged: a.onCreditFilterChanged,
											},
											dcl.PushButton{Text: "显示全部", MinSize: dcl.Size{Width: 80}, OnClicked: func() { a.setCreditFilter(creditAllUIDs) }},
											dcl.HSpacer{},
											dcl.Label{AssignTo: &a.lblCreditFilter, Text: ""},
										},
									},
									dcl.TableView{
										AssignTo:         &a.tvCredits,
										Model:            a.credits,
										AlternatingRowBG: true,
										StretchFactor:    1,
										Columns: []dcl.TableViewColumn{
											{Title: "时间", Width: 120},
											{Title: "账号", Width: 160},
											{Title: "变动", Width: 90, Alignment: dcl.AlignFar},
											{Title: "积分变化", Width: 170, Alignment: dcl.AlignFar},
											{Title: "来源", Width: 200},
										},
									},
								},
							},
							dcl.GroupBox{
								Title:         "运行日志 / 请求日志",
								Layout:        dcl.VBox{},
								StretchFactor: 2,
								Children: []dcl.Widget{
									dcl.TextEdit{
										AssignTo:      &a.teLog,
										ReadOnly:      true,
										VScroll:       true,
										HScroll:       true,
										StretchFactor: 1,
										Font:          dcl.Font{Family: "Cascadia Mono", PointSize: 9},
									},
								},
							},
						},
					},

					// ══════════════ 用量 ══════════════
					{
						Title:  "用量",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 10},
						Children: []dcl.Widget{
							dcl.Label{Text: "Token 用量统计（账号 × 模型 × 日期），数据来自上游响应里的 usage 字段。" +
								"\r\n· 只统计【成功请求】：失败请求（限流/余额不足）上游不返回 usage，仅计一次请求数。" +
								"\r\n· 缓存：命中缓存的输入 token。上下文很大但缓存命中率高时，实际计费输入远小于「输入」列。" +
								"\r\n· 本表是 token 维度的成本归因，不能用于核对积分扣减（那看「日志」页的积分记录）。"},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.Label{Text: "视图"},
									dcl.ComboBox{
										AssignTo:              &a.cbUsageScope,
										Model:                 []string{"账号 × 模型 明细", "账号级汇总"},
										CurrentIndex:          0,
										OnCurrentIndexChanged: a.refreshUsage,
									},
									dcl.Label{Text: "日期"},
									dcl.ComboBox{
										AssignTo:              &a.cbUsageDay,
										Model:                 []string{"全部日期"},
										CurrentIndex:          0,
										OnCurrentIndexChanged: a.refreshUsage,
									},
									dcl.PushButton{Text: "刷新", MinSize: dcl.Size{Width: 70}, OnClicked: a.refreshUsage},
									dcl.HSpacer{},
								},
							},
							dcl.Label{
								AssignTo: &a.lblUsageTotal,
								Text:     "—",
								Font:     dcl.Font{Family: "Segoe UI", PointSize: 11, Bold: true},
							},
							dcl.TableView{
								AssignTo:         &a.tvUsage,
								Model:            a.usage,
								AlternatingRowBG: true,
								StretchFactor:    1,
								Columns: []dcl.TableViewColumn{
									{Title: "账号", Width: 140},
									{Title: "模型", Width: 190},
									{Title: "日期", Width: 95},
									{Title: "请求数", Width: 70, Alignment: dcl.AlignFar},
									{Title: "缓存输入", Width: 90, Alignment: dcl.AlignFar},
									{Title: "输入", Width: 100, Alignment: dcl.AlignFar},
									{Title: "输出", Width: 100, Alignment: dcl.AlignFar},
									{Title: "其中思考", Width: 100, Alignment: dcl.AlignFar},
								},
							},
							dcl.Label{Text: "提示：双击「账号」页某行看该账号的积分变化；本表看 token 花在哪个账号/模型/日期。"},
						},
					},

					// ══════════════ 配置 ══════════════
					{
						Title:  "配置",
						Layout: dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 14, Right: 14, Bottom: 14}, Spacing: 10},
						Children: []dcl.Widget{
							dcl.GroupBox{
								Title:  "基础",
								Layout: dcl.Grid{Columns: 4, Spacing: 8},
								Children: []dcl.Widget{
									dcl.Label{Text: "监听地址"},
									dcl.LineEdit{AssignTo: &leListen, Text: getPath(raw, "listen"), ColumnSpan: 3},
									dcl.Label{Text: "API Key"},
									dcl.LineEdit{AssignTo: &leAPIKey, Text: getPath(raw, "api_key"), ColumnSpan: 3},
									dcl.Label{Text: "账号目录"},
									dcl.LineEdit{AssignTo: &leAuthDir, Text: getPath(raw, "auth_dir"), ColumnSpan: 3},
									dcl.Label{Text: "状态文件"},
									dcl.LineEdit{AssignTo: &leStateFile, Text: getPath(raw, "state_file"), ColumnSpan: 3},
								},
							},
							dcl.GroupBox{
								Title:  "调度与超时",
								Layout: dcl.Grid{Columns: 4, Spacing: 8},
								Children: []dcl.Widget{
									dcl.Label{Text: "签到时刻"},
									dcl.LineEdit{AssignTo: &leCheckinHours, Text: getPath(raw, "schedule", "checkin_hours")},
									dcl.Label{Text: "保活时刻"},
									dcl.LineEdit{AssignTo: &leKeepaliveHours, Text: getPath(raw, "schedule", "keepalive_hours")},
									dcl.Label{Text: "额度刷新间隔"},
									dcl.LineEdit{AssignTo: &leCreditRefresh, Text: getPath(raw, "schedule", "credit_refresh")},
									dcl.Label{Text: "短 RPC 超时"},
									dcl.LineEdit{AssignTo: &leTimeout, Text: getPath(raw, "upstream", "timeout_seconds")},
									dcl.Label{Text: "首字节超时"},
									dcl.LineEdit{AssignTo: &leHeaderTO, Text: getPath(raw, "upstream", "header_timeout_seconds")},
									dcl.Label{Text: "流空闲超时"},
									dcl.LineEdit{AssignTo: &leIdleTO, Text: getPath(raw, "upstream", "idle_timeout_seconds")},
									dcl.Label{Text: "软冷却时长"},
									dcl.LineEdit{AssignTo: &leSoftRate, Text: getPath(raw, "cooldown", "soft_rate")},
								},
							},
							dcl.GroupBox{
								Title:  "账号池与会话",
								Layout: dcl.Grid{Columns: 4, Spacing: 8},
								Children: []dcl.Widget{
									dcl.Label{Text: "单号在途上限"},
									dcl.LineEdit{AssignTo: &leMaxInFlight, Text: getPath(raw, "pool", "max_in_flight")},
									dcl.Label{Text: "熔断阈值"},
									dcl.LineEdit{AssignTo: &leBreakerTh, Text: getPath(raw, "pool", "breaker_threshold")},
									dcl.Label{Text: "熔断基础时长"},
									dcl.LineEdit{AssignTo: &leBreakerCd, Text: getPath(raw, "pool", "breaker_cooldown")},
									dcl.Label{Text: "熔断封顶"},
									dcl.LineEdit{AssignTo: &leBreakerCdMax, Text: getPath(raw, "pool", "breaker_cooldown_max")},
									dcl.Label{Text: "会话 TTL"},
									dcl.LineEdit{AssignTo: &leSessionTTL, Text: getPath(raw, "session_sticky", "ttl")},
									dcl.Label{Text: "会话 GC"},
									dcl.LineEdit{AssignTo: &leSessionGC, Text: getPath(raw, "session_sticky", "gc_interval")},
								},
							},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.CheckBox{
										AssignTo: &a.chkSticky,
										Text:     "启用会话粘性（同一会话尽量固定同一账号）",
										Checked:  getPath(raw, "session_sticky", "enabled") != "false",
									},
									dcl.HSpacer{},
								},
							},
							dcl.Composite{
								Layout: dcl.HBox{Spacing: 8},
								Children: []dcl.Widget{
									dcl.PushButton{Text: "保存", MinSize: dcl.Size{Width: 90}, OnClicked: func() { a.doSaveConfig(false) }},
									dcl.PushButton{Text: "保存并重启服务", MinSize: dcl.Size{Width: 140}, OnClicked: func() { a.doSaveConfig(true) }},
									dcl.HSpacer{},
								},
							},
							dcl.Label{AssignTo: &a.lblCfgHint, Text: "签到/保活时刻写法：[9,21] 表示每天 9 点与 21 点。"},
							dcl.VSpacer{},
						},
					},
				},
			},
		},
	}

	if err := mw.Create(); err != nil {
		return err
	}
	logf("主窗口已创建")

	// Create() 之后句柄才可用，回填配置表单引用
	a.ed = map[string]*walk.LineEdit{
		"listen": leListen, "api_key": leAPIKey, "auth_dir": leAuthDir, "state_file": leStateFile,
		"checkin_hours": leCheckinHours, "keepalive_hours": leKeepaliveHours,
		"credit_refresh": leCreditRefresh,
		"timeout":        leTimeout, "header_timeout": leHeaderTO, "idle_timeout": leIdleTO,
		"soft_rate": leSoftRate, "max_in_flight": leMaxInFlight,
		"breaker_threshold": leBreakerTh, "breaker_cooldown": leBreakerCd,
		"breaker_cooldown_max": leBreakerCdMax,
		"session_ttl":          leSessionTTL, "session_gc": leSessionGC,
	}

	a.chkAuto.SetChecked(autoStartEnabled())
	logf("自动启动开关已同步")

	// 停止检测按钮默认禁用（仅在检测进行中可用）。
	if a.btnMatrixStop != nil {
		a.btnMatrixStop.SetEnabled(false)
	}

	// 账号表选中变化 → 记录选中的 UID，供表格重建后恢复。
	// 必须绑定：用户用鼠标点击选行走的是 LVN_ITEMCHANGED，walk 内部的
	// currentItemID 不会更新，只有我们自己记录才能在重建后找回原行。
	a.tvAccounts.CurrentIndexChanged().Attach(a.rememberAccountSelection)

	// 模型表选中行 → 详情标签（展示完整参数，含表格放不下的字段）。
	a.tvModelRates.CurrentIndexChanged().Attach(func() {
		r, ok := a.modelRates.At(a.tvModelRates.CurrentIndex())
		if !ok {
			return
		}
		a.lblModelDetail.SetText(modelDetailText(r))
	})

	a.initTray()
	logf("托盘已就绪")
	return nil
}

// modelDetailText 把一行的完整参数拼成详情文本（含表格未直接展示的字段）。
func modelDetailText(r modelRateRow) string {
	cap := fmt.Sprintf("%d", r.ContextWindow)
	out := fmt.Sprintf("%d", r.MaxTokens)
	detail := fmt.Sprintf(
		"%s（%s）\r\n"+
			"思考深度：默认档 %s ｜ 可选档 %s ｜ 可关闭思考 %s ｜ 只能推理 %s\r\n"+
			"容量：上下文 %s tokens ｜ 最大输出 %s tokens\r\n"+
			"能力：图片输入 %s ｜ 工具调用 %s ｜ 推理 %s\r\n"+
			"计费：消耗倍率 %s\r\n"+
			"元信息：厂商 %s ｜ 标签 %s ｜ 默认模型 %s",
		r.ID, r.Name,
		r.Effort, r.Supported, r.CanDisable, r.OnlyReason,
		cap, out,
		r.Images, r.ToolCall, orDash(r.Reason),
		r.Rate,
		r.Vendor, r.Tags, r.IsDeflt,
	)
	if r.Desc != "" {
		detail += "\r\n说明：" + r.Desc
	}
	return detail
}

// initTray 系统托盘：关闭窗口时收进托盘，服务继续在后台跑。
func (a *app) initTray() {
	ni, err := walk.NewNotifyIcon(a.mw)
	if err != nil {
		logf("自检 · 托盘：创建失败（%v）", err)
		return
	}
	a.tray = ni

	if err := ni.SetIcon(a.icon); err != nil {
		logf("自检 · 托盘：设置图标失败（%v）", err)
	}
	if err := ni.SetToolTip(appName); err != nil {
		logf("自检 · 托盘：设置悬停提示失败（%v）", err)
	}

	show := walk.NewAction()
	_ = show.SetText("显示主界面")
	show.Triggered().Attach(func() { a.showWindow() })
	_ = ni.ContextMenu().Actions().Add(show)

	startA := walk.NewAction()
	_ = startA.SetText("启动服务")
	startA.Triggered().Attach(a.doStart)
	_ = ni.ContextMenu().Actions().Add(startA)

	stopA := walk.NewAction()
	_ = stopA.SetText("停止服务")
	stopA.Triggered().Attach(a.doStop)
	_ = ni.ContextMenu().Actions().Add(stopA)

	_ = ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	quit := walk.NewAction()
	_ = quit.SetText("退出")
	quit.Triggered().Attach(func() { a.quit() })
	_ = ni.ContextMenu().Actions().Add(quit)

	// 左键点击 → 显示/还原主窗口。
	// 用 MouseUp 而非 MouseDown：与 Windows 托盘惯例一致（按下不触发、松开才动作），
	// 也避免用户按下后拖开仍被当成点击。
	// 右键不在此处理——walk 的 notifyIconWndProc 已在 WM_RBUTTONUP 上弹出
	// ContextMenu（见 walk/notifyicon.go），保持默认行为。
	ni.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			a.showWindow()
		}
	})

	// SetVisible(true) 内部就是 Shell_NotifyIcon(NIM_ADD)：
	// 返回 nil 即代表图标已成功注册到通知区域。
	if err := ni.SetVisible(true); err != nil {
		a.trayOK = false
		logf("自检 · 托盘：注册失败（%v）", err)
	} else {
		a.trayOK = true
		logf("自检 · 托盘：图标已注册（Shell_NotifyIcon OK），右键菜单 4 项")
	}

	// 关闭窗口 = 收进托盘（网关继续跑）；真正退出走托盘菜单
	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if a.quitting {
			logf("自检 · 关闭：确认退出，开始清理")
			return
		}
		*canceled = true
		a.mw.Hide()
		logf("自检 · 关闭：窗口已收进托盘，进程与网关继续运行")
		if a.trayOK {
			_ = a.tray.ShowInfo(appName, "已收进托盘，网关继续在后台运行。右键托盘图标可退出。")
		}
	})
}

// showCommandFor 依据窗口当前状态返回 ShowWindow 命令。
//
// 为什么不能只用 walk 的 Show()：FormBase.Show → setWindowVisible(true) →
// ShowWindow(SW_SHOWNA)。SW_SHOWNA 的语义是"显示但不激活"，对【最小化】状态的
// 窗口不产生任何效果——窗口不会还原，用户点了托盘图标看不到反应（实测缺陷）。
// 因此最小化时必须显式用 SW_RESTORE。
func showCommandFor(minimized, visible bool) int32 {
	if minimized {
		return win.SW_RESTORE
	}
	return win.SW_SHOWNORMAL
}

// showWindow 从托盘把主窗口带回前台：还原（若最小化）→ 显示 → 置前。
func (a *app) showWindow() {
	if a.mw == nil {
		return
	}
	minimized := win.IsIconic(a.mw.Handle())
	if minimized {
		// 最小化：显式还原。SW_RESTORE 同时会激活窗口。
		win.ShowWindow(a.mw.Handle(), showCommandFor(minimized, true))
		return
	}
	// 未最小化：走 walk 的 Show()（内部 SW_SHOWNA）+ 置前 + 抢焦点，
	// 保持既有"收进托盘后再点出来"的行为不变。
	a.mw.Show()
	_ = a.mw.Activate()
	_ = a.mw.SetFocus()
}

func (a *app) quit() {
	a.quitting = true
	if a.tray != nil {
		_ = a.tray.SetVisible(false)
		_ = a.tray.Dispose()
	}
	if a.svc != nil {
		a.svc.Stop()
	}
	// 关闭积分历史的常开追加句柄（flush 到盘并释放文件锁）。
	// 必须在退出前做：Windows 下被占用的文件会阻止后续删除/重命名。
	if a.creditLog != nil {
		a.creditLog.Close()
	}
	// 关闭 gui.log 的轮转句柄：同理释放文件占用，也避免退出瞬间丢缓冲日志。
	if a.logRot != nil {
		_ = a.logRot.Close()
	}
	// token 用量统计落盘（内部 Flush；失败仅忽略——属观测数据）。
	if a.usageStore != nil {
		a.usageStore.Close()
	}
	if a.mw != nil {
		a.mw.Close()
	}
	walk.App().Exit(0)
}

// logf 写标准日志（已被 main 接到内存缓冲）。
func logf(format string, args ...any) {
	now := time.Now()
	fmt.Printf("%s %s\n", now.Format("15:04:05"), fmt.Sprintf(format, args...))
}
