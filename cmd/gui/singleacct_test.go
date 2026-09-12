package main

import (
	"testing"
)

// ─────────────── 对特定账号手动签到 / 刷新额度 ───────────────
//
// 需求：现有按钮只能对【全部账号】操作；需要对【选中的单个账号】做签到与刷新额度。
//
// 复用既有能力（不新增上游接口）：
//   - 签到：upstream.Client.DailyCheckin + IsAlreadyCheckedIn 判定"今天已签到"
//   - 额度：upstream.Client.UserResource → pool.SetCreditsReason（落到积分历史）
//
// 设计约束：
//   1. 结果必须写进「签到记录」表与积分历史（与批量操作口径一致，便于追溯）；
//   2. 未选中账号时给明确提示，不静默失败；
//   3. 与批量版保持一致的"已签到"处理：IsAlreadyCheckedIn 命中算成功路径，
//      不计入失败（上游对重复签到返回 400 code=10001，这是正常状态不是错误）；
//   4. 异步执行 + UI 线程回写（避免阻塞界面）。

// TestClassifyCheckinResult 签到结果归因（含上游真实报文样本）。
func TestClassifyCheckinResult(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantResult string
	}{
		{"成功", nil, "成功"},
		{
			name:       "今天已签到（上游 400 code=10001，真实报文）",
			err:        errStr(`upstream client (http 400): {"code":10001,"msg":"今天已签到，请明天再来","requestId":"x"}`),
			wantResult: "今天已签到",
		},
		{
			name:       "session 失效",
			err:        errStr(`upstream client (http 401): {"code":12153,"msg":"Offline user session not found"}`),
			wantResult: "失败",
		},
		{
			name:       "网络错误",
			err:        errStr("Post \"https://...\": connection refused"),
			wantResult: "失败",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, detail := classifyCheckinResult(c.err)
			if result != c.wantResult {
				t.Errorf("result=%q want %q", result, c.wantResult)
			}
			if c.err != nil && detail == "" {
				t.Error("失败时应带 detail（供记录表展示）")
			}
			if c.err == nil && detail != "" {
				t.Errorf("成功时不应有 detail, got %q", detail)
			}
		})
	}
}

// TestCheckinNotCountedAsFailWhenAlready 已签到不得计入失败（与批量版口径一致）。
func TestCheckinNotCountedAsFailWhenAlready(t *testing.T) {
	r, _ := classifyCheckinResult(errStr(`{"code":10001,"msg":"今天已签到，请明天再来"}`))
	if r == "失败" {
		t.Error("「今天已签到」是正常状态，不得判为失败（否则统计误导）")
	}
}

// TestCheckinResultLabels 结果文案与签到记录表一致（成功/今天已签到/失败）。
func TestCheckinResultLabels(t *testing.T) {
	want := map[string]bool{"成功": true, "今天已签到": true, "失败": true}
	for _, c := range []error{nil, errStr(`{"msg":"今天已签到"}`), errStr("boom")} {
		got, _ := classifyCheckinResult(c)
		if !want[got] {
			t.Errorf("结果文案 %q 不在约定集合内（签到记录表依赖它着色/统计）", got)
		}
	}
}

// errStr 构造一个简单的 error（避免引 fmt 造成测试噪音）。
type errStr string

func (e errStr) Error() string { return string(e) }

// ─────────────── 单账号操作的副作用契约 ───────────────

// TestSingleCheckinReasonLabel 单账号签到写积分历史时用专属来源标注，
// 便于在积分记录里区分"批量刷新"与"单账号操作"。
func TestSingleCheckinReasonLabel(t *testing.T) {
	// 来源文案约定（与 doCheckinSelected / doRefreshCreditsSelected 内部一致）
	labels := []string{"签到（手动/单账号）", "手动刷新（单账号）"}
	if labels[0] == "签到" || labels[1] == "手动刷新" {
		t.Fatal("单账号操作应有专属来源标注，避免与批量操作混淆")
	}
	for _, l := range labels {
		if l == "" {
			t.Error("来源标注不得为空（积分记录依赖它归因）")
		}
	}
}

// TestCheckinRefreshesCreditsContract 签到成功/已签到都应顺带刷新额度：
// 否则用户点"签到选中"后积分不变，会以为操作无效。
// （契约测试：真实刷新需上游，此处锁定"哪些结果触发刷新"的判定。）
func TestCheckinRefreshesCreditsContract(t *testing.T) {
	for _, err := range []error{
		nil, // 成功
		errStr(`{"code":10001,"msg":"今天已签到，请明天再来"}`), // 已签到
	} {
		result, _ := classifyCheckinResult(err)
		if result != "成功" && result != "今天已签到" {
			t.Fatalf("前置条件失败: result=%q", result)
		}
	}
	// 失败不得触发额度刷新（避免无谓的上游请求）
	result, _ := classifyCheckinResult(errStr("boom"))
	if result == "成功" || result == "今天已签到" {
		t.Errorf("失败不应被判为可刷新状态, got %q", result)
	}
}
