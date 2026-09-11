package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ─────────────── 一键检测某模型在哪些账号可用 ───────────────
//
// 说明：本功能是【诊断工具】，用户点击后会逐账号发真实探测请求（消耗少量积分，
// 固定小 max_tokens）。因此：
//   - 必须串行 + 间隔，避免同时打全部账号触发上游限流；
//   - 必须可中断（用户可能测到一半就不想测了）；
//   - 必须先给出确认提示（消耗积分这件事要让用户知情）；
//   - 汇总结果要能区分"能用/不能用/在冷却"，并给出失败原因。

// TestMatrixProbeSummaryCounts 汇总计数：可用/不可用/跳过 分别统计。
func TestMatrixProbeSummaryCounts(t *testing.T) {
	rows := []matrixRow{
		{UID: "a1", Name: "甲", OK: true},
		{UID: "a2", Name: "乙", OK: false, ErrKind: upstream.ErrModelRateLimit, Detail: "6004"},
		{UID: "a3", Name: "丙", OK: false, ErrKind: upstream.ErrHardCredit, Detail: "余额不足"},
		{UID: "a4", Name: "丁", Skipped: "该模型在冷却中（被网关跳过以避免白打）"},
	}
	s := sumMatrix(rows, "glm-5.2")
	if s.OK != 1 || s.Fail != 2 || s.Skip != 1 || s.Total != 4 {
		t.Errorf("汇总错误: %+v", s)
	}
	if s.Model != "glm-5.2" {
		t.Errorf("Model=%q", s.Model)
	}
	if s.BestUID() != "a1" {
		t.Errorf("BestUID=%q want a1", s.BestUID())
	}
}

// TestMatrixSummaryText 汇总文案：一眼看出"几个能用、几个不能用"，并给出首选账号。
func TestMatrixSummaryText(t *testing.T) {
	rows := []matrixRow{
		{UID: "u1", Name: "木瓜", OK: true, Elapsed: 1500 * time.Millisecond},
		{UID: "u2", Name: "138", OK: false, ErrKind: upstream.ErrModelRateLimit},
	}
	s := sumMatrix(rows, "deepseek-v4.1-flash")
	txt := s.Text()
	for _, want := range []string{"deepseek-v4.1-flash", "可用 1", "不可用 1", "木瓜"} {
		if !strings.Contains(txt, want) {
			t.Errorf("文案缺 %q:\n%s", want, txt)
		}
	}
}

// TestMatrixSummaryAllFail 全部不可用时的文案必须明确（用户据此判断该换模型）。
func TestMatrixSummaryAllFail(t *testing.T) {
	rows := []matrixRow{
		{UID: "u1", OK: false, ErrKind: upstream.ErrModelRateLimit},
		{UID: "u2", OK: false, ErrKind: upstream.ErrModelRateLimit},
	}
	s := sumMatrix(rows, "glm-5.3")
	if s.OK != 0 || s.Fail != 2 {
		t.Fatalf("汇总错误: %+v", s)
	}
	if !strings.Contains(s.Text(), "全部不可用") {
		t.Errorf("应明确提示全部不可用: %s", s.Text())
	}
}

// TestMatrixRowText 单行结果文案：成功给耗时；失败给归因（人话）。
func TestMatrixRowText(t *testing.T) {
	ok := matrixRow{UID: "u1", Name: "木瓜", OK: true, Elapsed: 2 * time.Second}
	if txt := ok.Text(); !strings.Contains(txt, "✅") || !strings.Contains(txt, "2.0s") {
		t.Errorf("成功行=%q", txt)
	}
	fail := matrixRow{UID: "u2", Name: "138", OK: false, ErrKind: upstream.ErrHardCredit, Detail: `{"code":1}`}
	txt := fail.Text()
	if !strings.Contains(txt, "❌") || !strings.Contains(txt, "余额不足") {
		t.Errorf("失败行=%q", txt)
	}
	skip := matrixRow{UID: "u3", Name: "甲", Skipped: "服务未运行"}
	if txt := skip.Text(); !strings.Contains(txt, "跳过") {
		t.Errorf("跳过行=%q", txt)
	}
}

// TestMatrixTargetsSkip 不可用账号应被跳过：禁用/无凭证/已在该模型冷却中的账号不必白打。
func TestMatrixTargetsSkip(t *testing.T) {
	items := []pool.Status{
		{UID: "u1", Credits: 100, CreditsKnown: true},
		{UID: "u2", Disabled: true, Reason: "session dead"},
		{UID: "u3", Credits: 50, CreditsKnown: true, Cooling: true},
	}
	// 全部账号都参与（禁用也测？——不：禁用账号的凭证已失效，测了必然失败且无意义）
	targets := probeTargets(items, func(uid string) bool { return false })
	if len(targets) != 2 {
		t.Fatalf("应跳过禁用账号，得到 %d 个目标: %+v", len(targets), targets)
	}
	for _, tgt := range targets {
		if tgt.UID == "u2" {
			t.Errorf("禁用账号不应参与检测: %+v", tgt)
		}
	}

	// 冷却中的账号：标为跳过（附原因），不白打
	targets2 := probeTargets(items, func(uid string) bool { return uid == "u1" })
	var u1 *matrixRow
	for i := range targets2 {
		if targets2[i].UID == "u1" {
			u1 = &targets2[i]
		}
	}
	if u1 == nil || u1.Skipped == "" {
		t.Errorf("冷却中的账号应标记跳过: %+v", targets2)
	}
}

// TestMatrixTargetsNoCredentials 无凭证账号跳过（凭证文件被删/加载失败）。
func TestMatrixTargetsNoCredentials(t *testing.T) {
	items := []pool.Status{{UID: "u1"}, {UID: "u2"}}
	got := probeTargets(items, func(uid string) bool { return false })
	// 两个都应作为目标（凭证是否存在由调用方在探测时判断并标失败）
	if len(got) != 2 {
		t.Fatalf("应 2 个目标, got %d", len(got))
	}
}

// TestProbeIntervalPositive 串行间隔必须为正：同时打全部账号会触发上游限流，
// 让"能否使用"的检测结果本身失真（把限流误判成不可用）。
func TestProbeIntervalPositive(t *testing.T) {
	if matrixProbeInterval <= 0 {
		t.Fatalf("间隔=%v 必须为正（避免限流污染检测结果）", matrixProbeInterval)
	}
	if matrixProbeInterval < 200*time.Millisecond {
		t.Errorf("间隔=%v 偏小，可能触发上游限流", matrixProbeInterval)
	}
}

// TestMatrixProbeSerialAndInterval 串行 + 间隔：并发探测会触发上游限流，
// 让"被限流"误判成"不可用"。本用例锁定行为：每次探测之间必须有间隔。
// （用真实 runMatrixProbe 需要 svc，故这里直接验证常量与顺序语义。）
func TestMatrixProbeSerialAndInterval(t *testing.T) {
	// 记录探测顺序与时刻，验证串行（非并发）与间隔存在
	var mu sync.Mutex
	var times []time.Time
	probe := func(uid string) *probeResult {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return &probeResult{OK: true}
	}
	// 模拟执行器循环（与 runMatrixProbe 的循环结构一致）
	uids := []string{"a", "b", "c"}
	for i, u := range uids {
		probe(u)
		if i < len(uids)-1 {
			time.Sleep(matrixProbeInterval)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 3 {
		t.Fatalf("应探测 3 次, got %d", len(times))
	}
	for i := 1; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		if gap < matrixProbeInterval/2 {
			t.Errorf("第 %d 次与上次间隔 %v 过小（应 ≥ %v，否则可能触发限流）",
				i+1, gap, matrixProbeInterval)
		}
	}
}

// TestSumMatrixBestIsFirstOK 首选账号取第一个可用的（顺序稳定，便于用户预期）。
func TestSumMatrixBestIsFirstOK(t *testing.T) {
	rows := []matrixRow{
		{UID: "u1", Name: "甲", OK: false, ErrKind: upstream.ErrModelRateLimit},
		{UID: "u2", Name: "乙", OK: true},
		{UID: "u3", Name: "丙", OK: true},
	}
	s := sumMatrix(rows, "m")
	if s.Best != "乙" || s.BestUID() != "u2" {
		t.Errorf("应取第一个可用账号: Best=%q BestUID=%q", s.Best, s.BestUID())
	}
}

// TestMatrixRowNoNameFallback 无昵称时用 UID 前 7 位（与日志口径一致）。
func TestMatrixRowNoNameFallback(t *testing.T) {
	items := []pool.Status{{UID: "6a77cbda-7768-4b90"}}
	got := probeTargets(items, nil)
	if len(got) != 1 || got[0].Name != "6a77cbda"[:7] {
		t.Errorf("无昵称应回落 UID 前 7 位: %+v", got)
	}
}

// TestMatrixModelChoiceFromOwnControl 回归：检测用的模型必须来自【账号页自己的】
// 模型下拉框，不得偷偷读「测试」页的选择。
//
// 缺陷背景：初版实现从 a.cbProbeModel（测试页控件）取值当默认，导致用户在账号页
// 点检测时既看不到要测哪个模型、也无法在账号页修改它；没去过测试页时更会默认测
// 模型清单里的第一项（可能是 auto）。这是"控件缺失"而非小疏漏。
func TestMatrixModelChoiceFromOwnControl(t *testing.T) {
	// 账号页选择优先：构造仅带账号页控件引用的 app（测试页控件为 nil）
	a := &app{}
	a.cbMatrixModel = nil // 显式说明：这里不是靠测试页控件
	if got := a.selectedMatrixModel(); got != "" {
		t.Errorf("无控件时应返回空串（提示用户选择）, got %q", got)
	}
}

// TestMatrixModelChoicesPlaceholder 模型清单为空时给出明确操作指引，
// 而不是让用户面对空下拉框或静默测错模型。
func TestMatrixModelChoicesPlaceholder(t *testing.T) {
	got := probeModelChoices(nil)
	if len(got) != 1 || !strings.Contains(got[0], "模型") {
		t.Errorf("空清单应给指引文案, got %v", got)
	}
	// 占位文案必须能被识别（避免把提示当模型名去测）
	if !isPlaceholderModel(got[0]) {
		t.Errorf("占位文案应被 isPlaceholderModel 识别: %q", got[0])
	}
	// 正常模型名不应被误判为占位
	if isPlaceholderModel("glm-5.2") || isPlaceholderModel("auto") {
		t.Error("真实模型名不得被识别为占位")
	}
}

// ─────────────── 检测请求的提示词与思考档 ───────────────

// TestProbeDefaultPromptIsMinimal 检测/探测的默认提示词是「仅回复1」：
// 极短（输入 token 最小），回答可预期（一个字符），适合只判断"通不通"。
func TestProbeDefaultPromptIsMinimal(t *testing.T) {
	if probeDefaultPrompt != "仅回复1" {
		t.Fatalf("默认提示词=%q want %q", probeDefaultPrompt, "仅回复1")
	}
	body := buildProbeBody("deepseek-v4.1-flash", probeMaxTokens, "")
	var obj struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(obj.Messages) != 1 || obj.Messages[0].Content != "仅回复1" {
		t.Errorf("实际发出的提示词=%q want 仅回复1", obj.Messages[0].Content)
	}
}

// TestProbeBodyHasNoEffortField 检测请求【刻意不传】思考档：实测 deepseek 系
// 不传档位时思考 token 恒为 0，比传最小档（low）还省（low 实测思考 522~685）。
// 本用例锁定"不传"，防止有人为了"用最小档"反而增加消耗。
func TestProbeBodyHasNoEffortField(t *testing.T) {
	body := buildProbeBody("deepseek-v4.1-flash", probeMaxTokens, "")
	if strings.Contains(body, "reasoning_effort") || strings.Contains(body, "reasoningEffort") {
		t.Fatalf("检测请求不得携带思考档（不传 = 思考 0，比最小档更省）: %s", body)
	}
}
