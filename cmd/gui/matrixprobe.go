// matrixprobe.go — 一键检测某模型在哪些账号可用。
//
// 用途：上游对模型做账号级限制时（model_rate_limit / 额度差异），需要快速回答
// "这个模型在哪些账号上还能用"。逐账号发一次小请求即可得到确定答案。
//
// ⚠️ 本功能会消耗积分：每次检测 = 账号数 × 1 个真实请求。
// 请求固定 probeMaxTokens（32）且不带思考档，单次消耗极小；但账号多时仍会累积。
// 因此：调用前必须让用户确认，检测过程串行 + 间隔（避免触发上游限流让结果失真），
// 且支持中途取消。
//
// 与「测试」页的关系：测试页是"指定一个账号×模型"的手动探测；本功能是
// "指定一个模型，横向扫全部账号"，输出一张可用性矩阵。
package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// matrixProbeInterval 每个账号之间的间隔。
// 必须为正：同时打全部账号会触发上游频率限制，导致"被限流"被误判成"该账号不可用"——
// 检测结果本身失真比慢几秒严重得多（与签到/额度刷新的 200ms 节奏同口径）。
const matrixProbeInterval = 300 * time.Millisecond

// matrixRow 单个账号的检测结果。
type matrixRow struct {
	UID     string
	Name    string
	OK      bool
	ErrKind upstream.ErrKind
	Detail  string
	Elapsed time.Duration
	// Skipped 非空表示未实际探测，值为跳过原因（如账号已禁用、模型正在冷却）。
	Skipped string
}

// Text 单行结果文案。
func (r matrixRow) Text() string {
	if r.Skipped != "" {
		return fmt.Sprintf("⏭ %s：跳过（%s）", r.Name, r.Skipped)
	}
	if r.OK {
		return fmt.Sprintf("✅ %s：可用（%.1fs）", r.Name, r.Elapsed.Seconds())
	}
	return fmt.Sprintf("❌ %s：不可用（%s）", r.Name, probeFailureText(r.ErrKind, r.Detail))
}

// matrixSummary 检测汇总。
type matrixSummary struct {
	Model string
	OK    int
	Fail  int
	Skip  int
	Total int
	// Best 第一个可用账号的展示名（用于提示"优先用哪个"）。
	Best string
	// bestUID 第一个可用账号的 UID（供程序化使用，避免调用方再去反查名字）。
	bestUID string
}

// sumMatrix 汇总检测结果。
func sumMatrix(rows []matrixRow, model string) matrixSummary {
	s := matrixSummary{Model: model, Total: len(rows)}
	for _, r := range rows {
		switch {
		case r.Skipped != "":
			s.Skip++
		case r.OK:
			s.OK++
			if s.bestUID == "" {
				s.bestUID = r.UID
				s.Best = r.Name
				if s.Best == "" {
					s.Best = r.UID
				}
			}
		default:
			s.Fail++
		}
	}
	return s
}

// BestUID 返回第一个可用账号的 UID（供程序化使用）。
func (s matrixSummary) BestUID() string { return s.bestUID }

// Text 汇总文案：一眼看出"几个能用、几个不能用"，并给出首选账号。
func (s matrixSummary) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "模型 %s：可用 %d · 不可用 %d", s.Model, s.OK, s.Fail)
	if s.Skip > 0 {
		fmt.Fprintf(&b, " · 跳过 %d", s.Skip)
	}
	fmt.Fprintf(&b, "（共 %d 个账号）", s.Total)
	switch {
	case s.OK == 0 && s.Fail > 0:
		b.WriteString("\n该模型全部不可用——建议换模型，或等冷却恢复后重测。")
	case s.OK > 0 && s.Best != "":
		fmt.Fprintf(&b, "\n优先使用：%s", s.Best)
	}
	return b.String()
}

// probeTargets 生成待检测的账号列表。
//   - 已禁用账号直接排除（凭证已失效，测了必然失败且没有诊断价值）；
//   - isModelCooling(uid) 为真时标为"跳过"（该模型正在冷却，网关本来就不会用它，
//     此时探测会被上游拒绝，结果会把"冷却中"误报成"不可用"）。
//
// 返回的切片同时包含待探测项与已跳过项（跳过项供界面展示原因）。
func probeTargets(items []pool.Status, isModelCooling func(uid string) bool) []matrixRow {
	out := make([]matrixRow, 0, len(items))
	for _, st := range items {
		name := st.Nickname
		if strings.TrimSpace(name) == "" {
			name = shortUID(st.UID)
		}
		if st.Disabled {
			continue // 禁用账号不参与（凭证失效，无诊断意义）
		}
		if isModelCooling != nil && isModelCooling(st.UID) {
			out = append(out, matrixRow{UID: st.UID, Name: name, Skipped: "该模型正在冷却中，避免误报为不可用"})
			continue
		}
		out = append(out, matrixRow{UID: st.UID, Name: name})
	}
	return out
}

// selectedMatrixModel 返回账号页模型下拉框当前选中的模型 ID；未选/占位提示返回空串。
func (a *app) selectedMatrixModel() string {
	if a.cbMatrixModel == nil {
		return ""
	}
	idx := a.cbMatrixModel.CurrentIndex()
	if idx < 0 {
		return ""
	}
	m := strings.TrimSpace(a.cbMatrixModel.Text())
	if isPlaceholderModel(m) {
		return ""
	}
	return m
}

// isPlaceholderModel 识别下拉框里的"提示文案"（不是真实模型名），
// 避免把提示当成模型去发请求。
func isPlaceholderModel(s string) bool {
	return strings.Contains(s, "请先到") || strings.Contains(s, "重新加载") || strings.Contains(s, "（")
}

// shortUID 无昵称时的展示名（前 7 位，与日志口径一致）。
func shortUID(uid string) string {
	if len(uid) > 7 {
		return uid[:7]
	}
	return uid
}

// ─────────────────────────── 执行器 ───────────────────────────

// runMatrixProbe 逐账号检测指定模型是否可用。
//
// 约束：
//   - 串行 + matrixProbeInterval 间隔：并发打全部账号会触发上游限流，
//     把"限流"误判成"该账号不可用"，检测结果失真；
//   - 每步回调 onStep（供界面增量显示进度，UI 层负责投递到 UI 线程）；
//   - 支持取消（cancel 返回 true 时立即停止，已测结果保留）；
//   - 复用测试页的 runProbe（同一套请求体与错误归因，口径一致）。
func runMatrixProbe(a *app, model string, cancel func() bool, onStep func(matrixRow)) []matrixRow {
	items := a.svc.Accounts()
	cooling := func(uid string) bool { return a.svc.IsModelCooling(uid, model) }
	full := probeTargets(items, cooling)

	// 待探测项（跳过项直接回调，不消耗请求）
	rows := make([]matrixRow, 0, len(full))
	for _, r := range full {
		if r.Skipped != "" {
			rows = append(rows, r)
			if onStep != nil {
				onStep(r)
			}
		}
	}
	var pending []matrixRow
	for _, r := range full {
		if r.Skipped == "" {
			pending = append(pending, r)
		}
	}

	for i, r := range pending {
		if cancel != nil && cancel() {
			// 取消：剩余项标为跳过（用户可见"没测完"），已测结果保留。
			for _, rest := range pending[i:] {
				rest.Skipped = "已取消"
				rows = append(rows, rest)
				if onStep != nil {
					onStep(rest)
				}
			}
			break
		}
		res := runProbe(a, r.UID, model, probeMaxTokens, "")
		r.OK = res.OK
		r.ErrKind = res.ErrKind
		r.Detail = res.Detail
		r.Elapsed = res.Elapsed
		rows = append(rows, r)
		if onStep != nil {
			onStep(r)
		}
		if i < len(pending)-1 {
			time.Sleep(matrixProbeInterval) // 防限流：串行 + 间隔
		}
	}
	return rows
}

// syncMatrixModels 刷新账号页的模型下拉框（模型清单加载后调用）。
// 与其它下拉框同策略：选项未变不重建，避免打断用户选择。
func (a *app) syncMatrixModels() {
	if a.cbMatrixModel == nil {
		return
	}
	models := probeModelChoices(a.modelRates.items)
	if equalStrs(models, a.lastMatrixModels) {
		return
	}
	a.lastMatrixModels = models
	if err := a.cbMatrixModel.SetModel(models); err != nil {
		logf("账号页模型下拉框刷新失败: %v", err)
		return
	}
	if len(models) > 0 {
		a.cbMatrixModel.SetCurrentIndex(0)
	}
}

// ─────────────────────────── UI 接线 ───────────────────────────

// doMatrixProbe 账号页「检测模型可用性」入口：
// 选模型 → 确认消耗 → 逐账号探测 → 结果写入「测试」页的结果区。
func (a *app) doMatrixProbe() {
	if a.matrixBusy {
		a.lblAccts2.SetText("检测进行中，请稍候…（可点「停止检测」中断）")
		return
	}
	if !a.svc.Running() {
		a.lblAccts2.SetText("服务未运行：请先到「服务」页点「启动服务」")
		return
	}
	items := a.svc.Accounts()
	if len(items) == 0 {
		a.lblAccts2.SetText("没有账号可检测")
		return
	}
	// 模型一律取自【账号页自己的下拉框】：用户在点击前就能看到并修改要测哪个模型。
	// （初版从「测试」页控件取值是错的——账号页上看不见也改不了。）
	def := a.selectedMatrixModel()
	if def == "" {
		if a.modelRates == nil || len(a.modelRates.items) == 0 {
			a.lblAccts2.SetText("模型清单为空：请先到「模型」页点「重新加载参数」")
		} else {
			a.lblAccts2.SetText("请先在账号页选择要检测的模型")
		}
		return
	}
	// 明确告知消耗：每次检测 = 账号数 × 1 个真实请求。
	n := 0
	for _, st := range items {
		if !st.Disabled {
			n++
		}
	}
	if walk.MsgBox(a.mw, appName,
		fmt.Sprintf("将逐账号检测模型 %s 是否可用。\n\n"+
			"· 会发起约 %d 次真实请求（每个账号 1 次，max_tokens=32，消耗极小但会消耗积分）\n"+
			"· 串行执行并带间隔，避免触发上游限流导致误判\n"+
			"· 期间可点「停止检测」中断\n\n继续吗？", def, n),
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		a.lblAccts2.SetText("已取消检测")
		return
	}

	a.matrixBusy = true
	a.matrixCancel = false
	if a.btnMatrixStop != nil {
		a.btnMatrixStop.SetEnabled(true)
	}
	// 切到「测试」页看结果（那里有结果区，且不与账号表抢空间）
	if a.tabs != nil {
		_ = a.tabs.SetCurrentIndex(a.tabIndexByTitle("测试"))
	}
	a.appendProbeLine(fmt.Sprintf("═══ 开始检测：模型 %s × %d 个账号 ═══", def, n))

	go func() {
		rows := runMatrixProbe(a, def, func() bool { return a.matrixCancel }, func(r matrixRow) {
			a.mw.Synchronize(func() { a.appendProbeLine("  " + r.Text()) })
		})
		sum := sumMatrix(rows, def)
		a.mw.Synchronize(func() {
			a.matrixBusy = false
			if a.btnMatrixStop != nil {
				a.btnMatrixStop.SetEnabled(false)
			}
			a.appendProbeLine("═══ 检测完成 ═══")
			for _, line := range strings.Split(sum.Text(), "\n") {
				a.appendProbeLine(line)
			}
			a.lblAccts2.SetText(fmt.Sprintf("检测完成：可用 %d / 不可用 %d / 跳过 %d", sum.OK, sum.Fail, sum.Skip))
		})
	}()
}

// doMatrixProbeSelected 账号页「检测选中」入口：只对【选中的那个账号】检测所选模型。
// 与 doMatrixProbe（全量横向扫）互补：怀疑单个账号在某模型上有问题时，
// 不必扫全部账号（省积分、快）。消耗 = 1 次真实请求（max_tokens=32）。
func (a *app) doMatrixProbeSelected() {
	if a.matrixBusy {
		a.lblAccts2.SetText("检测进行中，请稍候…（可点「停止检测」中断）")
		return
	}
	if !a.svc.Running() {
		a.lblAccts2.SetText("服务未运行：请先到「服务」页点「启动服务」")
		return
	}
	def := a.selectedMatrixModel()
	if def == "" {
		if a.modelRates == nil || len(a.modelRates.items) == 0 {
			a.lblAccts2.SetText("模型清单为空：请先到「模型」页点「重新加载参数」")
		} else {
			a.lblAccts2.SetText("请先在账号页选择要检测的模型")
		}
		return
	}
	items := a.svc.Accounts()
	a.takeSelection(func(idx int) {
		if idx < 0 || idx >= len(items) {
			a.lblAccts2.SetText("请先在表格里选中一个账号")
			return
		}
		st := items[idx]
		if st.Disabled {
			a.lblAccts2.SetText("该账号已禁用，不参与检测")
			return
		}
		name := a.displayName(st.UID)
		// 单账号检测也确认一次：虽然是 1 次小请求，但会真实消耗积分（用户须知情）。
		if walk.MsgBox(a.mw, appName,
			fmt.Sprintf("将用账号 %s 检测模型 %s 是否可用。\n\n"+
				"· 发起 1 次真实请求（max_tokens=32，消耗极小但会消耗积分）\n\n继续吗？", name, def),
			walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
			a.lblAccts2.SetText("已取消检测")
			return
		}

		a.matrixBusy = true
		if a.btnMatrixStop != nil {
			a.btnMatrixStop.SetEnabled(true)
		}
		a.appendProbeLine(fmt.Sprintf("═══ 检测选中账号：%s × 模型 %s ═══", name, def))
		go func() {
			res := runProbe(a, st.UID, def, probeMaxTokens, "")
			a.mw.Synchronize(func() {
				a.matrixBusy = false
				if a.btnMatrixStop != nil {
					a.btnMatrixStop.SetEnabled(false)
				}
				a.appendProbeLine("  " + res.Summary())
				a.appendProbeLine("═══ 检测完成 ═══")
				if res.OK {
					a.lblAccts2.SetText(fmt.Sprintf("%s：模型 %s 可用（%.1fs）", name, def, res.Elapsed.Seconds()))
				} else {
					a.lblAccts2.SetText(fmt.Sprintf("%s：模型 %s 不可用（%s）", name, def, firstLine(probeFailureText(res.ErrKind, res.Detail))))
				}
			})
		}()
	})
}

// doStopMatrixProbe 中断进行中的检测（已测结果保留）。
func (a *app) doStopMatrixProbe() {
	if !a.matrixBusy {
		a.lblAccts2.SetText("当前没有进行中的检测")
		return
	}
	a.matrixCancel = true
	a.lblAccts2.SetText("正在停止…（当前这一条测完即止）")
}
