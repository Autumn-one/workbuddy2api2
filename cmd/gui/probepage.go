// probepage.go — 「测试」页逻辑：选定账号+模型，手动探测上游连通性。
//
// 用途：怀疑某个账号/模型有问题时，绕过网关轮换，用【指定账号的凭证】直接打一次
// 上游，定位"是账号的问题还是模型的问题"。请求固定小 max_tokens 省积分。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// probeMaxTokens 探测请求的默认输出上限：足够返回一句话，又把积分消耗压到最低。
const probeMaxTokens = 32

// probeDefaultPrompt 探测/检测的默认提示词。
//
// 取值原则（都是"把消耗压到最低 + 结论可预期"）：
//   - 极短：「仅回复1」只有 3 个字符，输入 token 接近下限；
//   - 回答可预期：要求只回一个字符，输出 token 稳定在 1~3，不会因模型"话多"抖动。
//
// 检测功能只用它，不接受自定义提示词（那是「测试」页的能力）。
const probeDefaultPrompt = "仅回复1"

// probeMaxTokensLimit 用户自定义输出上限的硬顶：防误填超大值意外烧积分。
const probeMaxTokensLimit = 4096

// probeResult 一次探测的结果。
type probeResult struct {
	OK      bool
	Elapsed time.Duration
	InTok   int // usage.prompt_tokens；-1 = 上游未报
	OutTok  int // usage.completion_tokens；-1 = 上游未报
	ErrKind upstream.ErrKind
	Detail  string // 上游原始返回片段 / 网络错误
}

// Summary 把结果压成一行人话。
func (r *probeResult) Summary() string {
	var b strings.Builder
	if r.OK {
		b.WriteString("✅ 成功")
	} else {
		b.WriteString("❌ 失败")
	}
	fmt.Fprintf(&b, " · 耗时 %.1fs", r.Elapsed.Seconds())
	if r.InTok >= 0 || r.OutTok >= 0 {
		fmt.Fprintf(&b, " · in=%s out=%s", probeIntOrDash(r.InTok), probeIntOrDash(r.OutTok))
	}
	if !r.OK {
		fmt.Fprintf(&b, " · %s", probeFailureText(r.ErrKind, r.Detail))
	}
	return b.String()
}

// probeIntOrDash token 计数转显示；-1（上游未报）显示 "-"。
func probeIntOrDash(n int) string {
	if n < 0 {
		return "-"
	}
	return itoa(int64(n))
}

// probeFailureText 把错误分类翻译成人话，附上原始返回片段（截断）。
func probeFailureText(kind upstream.ErrKind, detail string) string {
	var why string
	switch kind {
	case upstream.ErrModelRateLimit:
		why = "模型级限流（6004）"
	case upstream.ErrHardCredit:
		why = "余额不足"
	case upstream.ErrSessionDead:
		why = "会话失效（需重新登录）"
	case upstream.ErrSoftRate:
		why = "频率限制"
	case upstream.ErrNotFound:
		why = "接口 404"
	case upstream.ErrServer:
		why = "上游 5xx"
	case upstream.ErrClient:
		why = "请求被上游拒绝"
	case upstream.ErrContextOverflow:
		why = "输入超出模型上下文上限"
	default:
		why = "未知错误"
	}
	detail = strings.TrimSpace(detail)
	if detail != "" {
		why += "：" + truncateLogLine(detail)
	}
	return why
}

// buildProbeBody 组装探测请求体：单条 user 消息 + stream:true（上游拒绝非流式）
// + max_tokens 上限。prompt 为空/纯空白时回落 probeDefaultPrompt；否则原文透传
// （用户可能用提示词验证特定能力，如"请输出一段很长的回复"测长文、JSON 输出测工具格式）。
// 刻意【不带】reasoning_effort：探测关心的是"通不通"，不是思考质量。
// 关键实测依据（见 使用指南.md）：deepseek 系模型不传档位时【思考 token 恒为 0】，
// 比传最小档 low 还省（low 实测思考 522~685 token）。因此"不传"才是真正的
// "思考深度最小"——不要为了"用最小档"改成传 low，那会让消耗显著增加。
func buildProbeBody(model string, maxTokens int, prompt string) string {
	if maxTokens <= 0 {
		maxTokens = probeMaxTokens
	}
	content := strings.TrimSpace(prompt)
	if content == "" {
		content = probeDefaultPrompt
	}
	obj := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
		"stream":     true,
		"max_tokens": maxTokens,
	}
	raw, _ := json.Marshal(obj)
	return string(raw)
}

// probeModelChoices 模型下拉框选项：来自「模型」页已加载的实时表。
// 空表给占位提示，避免用户面对一个空下拉框不知所措。
func probeModelChoices(rows []modelRateRow) []string {
	if len(rows) == 0 {
		return []string{"（请先到「模型」页点「重新加载参数」）"}
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ID != "" {
			out = append(out, r.ID)
		}
	}
	return out
}

// probeAccountLabel 下拉框条目：昵称（无则 UID 前 7 位）+ 状态标注。
func probeAccountLabel(nickname, uid, state string) string {
	name := strings.TrimSpace(nickname)
	if name == "" {
		if len(uid) > 7 {
			name = uid[:7]
		} else {
			name = uid
		}
	}
	if state != "" && state != "正常" {
		return fmt.Sprintf("%s（%s）", name, state)
	}
	return name
}

// runProbe 用指定账号的凭证直接打一次上游 chat（绕过网关轮换）。
// 同步执行——调用方放在 goroutine 里。结果保证非 nil。
func runProbe(a *app, uid, model string, maxTokens int, prompt string) *probeResult {
	start := time.Now()
	res := &probeResult{InTok: -1, OutTok: -1}

	if a.svc == nil {
		res.Elapsed = time.Since(start)
		res.Detail = "内部错误：服务对象未初始化"
		return res
	}
	up := a.svc.Upstream()
	if up == nil {
		res.Elapsed = time.Since(start)
		res.Detail = "服务未运行（先到「服务」页启动）"
		return res
	}
	acct := a.svc.AuthByUID(uid)
	if acct == nil {
		res.Elapsed = time.Since(start)
		res.Detail = "账号不在池中（可能已被删除，刷新一下账号页）"
		return res
	}

	rc, status, respBody, params, err := up.ChatStreamWithParams(acct, []byte(buildProbeBody(model, maxTokens, prompt)))
	res.Elapsed = time.Since(start)
	if err != nil {
		res.Detail = err.Error()
		return res
	}
	if rc != nil {
		defer rc.Close()
	}
	if status >= 400 {
		res.ErrKind = upstream.Classify(status, string(respBody))
		res.Detail = string(respBody)
		// usage 在失败响应里通常没有，保持 -1
		return res
	}

	// 成功：聚合流拿 usage 与内容
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		res.ErrKind = upstream.ErrNone
		res.Detail = "流解析失败：" + err.Error()
		return res
	}
	res.OK = true
	res.InTok = promptTokensOf(resp)
	res.OutTok = completionTokensOf(resp)
	_ = params // 请求参数快照暂不展示，保持结果行简洁
	return res
}

// promptTokensOf / completionTokensOf：从聚合响应提取 usage；缺失返回 -1。
func promptTokensOf(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["prompt_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

func completionTokensOf(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// ─────────────────────────── UI 接线 ───────────────────────────

// syncProbeChoices 刷新测试页的两个下拉框（账号增删/模型表加载后调用）。
// 只在选项真正变化时重建，避免打断用户当前选择（与积分过滤下拉框同策略）。
func (a *app) syncProbeChoices() {
	if a.cbProbeAcct == nil || a.cbProbeModel == nil {
		return
	}
	// 账号：昵称（状态）标注；索引平行记录 uid
	acctNames := make([]string, 0, 8)
	uids := make([]string, 0, 8)
	for _, st := range a.svc.Accounts() {
		acctNames = append(acctNames, probeAccountLabel(st.Nickname, st.UID, accountState(st)))
		uids = append(uids, st.UID)
	}
	// 模型：固定取 cli 可对话清单（非 cli 模型只供展示，探测它们只会报错/白耗积分）
	models := probeModelChoices(a.cliRows)

	uidChanged := !equalStrs(uids, a.probeUIDs)
	modelChanged := !equalStrs(models, a.lastProbeModels)
	if !uidChanged && !modelChanged {
		return
	}
	if uidChanged {
		a.probeUIDs = uids
		if err := a.cbProbeAcct.SetModel(acctNames); err != nil {
			log.Printf("测试页账号下拉框刷新失败: %v", err)
		}
		if len(uids) > 0 {
			a.cbProbeAcct.SetCurrentIndex(0)
		}
	}
	if modelChanged {
		a.lastProbeModels = models
		if err := a.cbProbeModel.SetModel(models); err != nil {
			log.Printf("测试页模型下拉框刷新失败: %v", err)
		}
		if len(models) > 0 {
			a.cbProbeModel.SetCurrentIndex(0)
		}
	}
}

// selectedProbeUID 返回当前选中的账号 uid；未选中返回空串。
func (a *app) selectedProbeUID() string {
	if a.cbProbeAcct == nil {
		return ""
	}
	idx := a.cbProbeAcct.CurrentIndex()
	if idx < 0 || idx >= len(a.probeUIDs) {
		return ""
	}
	return a.probeUIDs[idx]
}

// selectedProbeModel 返回当前选中的模型 ID；未选中返回空串。
func (a *app) selectedProbeModel() string {
	if a.cbProbeModel == nil {
		return ""
	}
	idx := a.cbProbeModel.CurrentIndex()
	if idx < 0 {
		return ""
	}
	return a.cbProbeModel.Text()
}

// doProbe 执行一次探测：UI 线程取参 → goroutine 打上游 → 回 UI 线程写结果。
// 期间禁用按钮防重复点击。
func (a *app) doProbe() {
	if a.probeBusy {
		return
	}
	uid := a.selectedProbeUID()
	model := strings.TrimSpace(a.selectedProbeModel())
	if uid == "" {
		a.appendProbeLine("请先选择账号（没有账号？先到「登录」页添加）")
		return
	}
	if model == "" || strings.Contains(model, "重新加载") {
		a.appendProbeLine("请先选择模型（模型列表为空？先到「模型」页点「重新加载参数」）")
		return
	}
	if !a.svc.Running() {
		a.appendProbeLine("服务未运行：请先到「服务」页点「启动服务」")
		return
	}

	// 自定义输出上限：空/非法回落默认 32；给个合理上界防误填超大值烧积分。
	maxTokens := probeMaxTokens
	if v, err := strconv.Atoi(strings.TrimSpace(a.leProbeTokens.Text())); err == nil && v > 0 {
		maxTokens = v
		if maxTokens > probeMaxTokensLimit {
			maxTokens = probeMaxTokensLimit
		}
	}
	prompt := a.leProbePrompt.Text()

	a.probeBusy = true
	a.btnProbe.SetEnabled(false)
	acctName := a.displayName(uid)
	a.appendProbeLine(fmt.Sprintf("── %s × %s 探测中…（max_tokens=%d）", acctName, model, maxTokens))

	go func() {
		res := runProbe(a, uid, model, maxTokens, prompt)
		a.mw.Synchronize(func() {
			a.probeBusy = false
			if a.btnProbe != nil {
				a.btnProbe.SetEnabled(true)
			}
			a.appendProbeLine(fmt.Sprintf("%s × %s：%s", acctName, model, res.Summary()))
			if !res.OK && res.Detail != "" {
				// 失败详情另起一行，便于完整查看上游报文
				a.appendProbeLine("    ↳ " + truncateLogLine(strings.ReplaceAll(res.Detail, "\n", " ")))
			}
		})
	}()
}

// appendProbeLine 往测试页结果区追加一行（UI 线程调用）。
func (a *app) appendProbeLine(line string) {
	if a.teProbe == nil {
		return
	}
	a.teProbe.AppendText(time.Now().Format("15:04:05 ") + line + "\r\n")
}
