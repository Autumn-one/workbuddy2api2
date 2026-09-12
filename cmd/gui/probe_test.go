package main

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// TestBuildProbeBody 测试请求体：小 max_tokens 省积分，强制 stream（上游拒绝非流式）。
func TestBuildProbeBody(t *testing.T) {
	body := buildProbeBody("glm-5.2", 0, "")
	if !strings.Contains(body, `"model":"glm-5.2"`) {
		t.Errorf("body 缺 model: %s", body)
	}
	if !strings.Contains(body, `"stream":true`) {
		t.Errorf("body 必须 stream:true（上游拒绝非流式）: %s", body)
	}
	if !strings.Contains(body, "max_tokens") {
		t.Errorf("body 应带 max_tokens 上限: %s", body)
	}
	if strings.Contains(body, "reasoning_effort") {
		t.Errorf("探测请求不应带思考档（省积分、快返回）: %s", body)
	}
	// messages 必须存在且为一条 user
	if !strings.Contains(body, `"role":"user"`) {
		t.Errorf("body 应含 user 消息: %s", body)
	}
}

// TestProbeResultSummary 结果摘要：成功/失败都要能一眼看懂。
func TestProbeResultSummary(t *testing.T) {
	ok := &probeResult{OK: true, Elapsed: 1500 * time.Millisecond, InTok: 12, OutTok: 8}
	if s := ok.Summary(); !strings.Contains(s, "成功") || !strings.Contains(s, "1.5s") {
		t.Errorf("成功摘要=%q", s)
	}
	fail := &probeResult{OK: false, Elapsed: 800 * time.Millisecond, ErrKind: upstream.ErrModelRateLimit, Detail: "6004 限流"}
	if s := fail.Summary(); !strings.Contains(s, "失败") || !strings.Contains(s, "6004 限流") {
		t.Errorf("失败摘要=%q", s)
	}
}

// TestClassifyProbeFailure 失败归因：把上游错误翻译成人话。
func TestClassifyProbeFailure(t *testing.T) {
	cases := []struct {
		kind upstream.ErrKind
		want string
	}{
		{upstream.ErrModelRateLimit, "模型级限流"},
		{upstream.ErrHardCredit, "余额不足"},
		{upstream.ErrSessionDead, "会话失效"},
		{upstream.ErrSoftRate, "频率限制"},
		{upstream.ErrNotFound, "接口 404"},
		{upstream.ErrServer, "上游 5xx"},
		{upstream.ErrClient, "请求被上游拒绝"},
		{upstream.ErrContextOverflow, "输入超出模型上下文上限"},
	}
	for _, c := range cases {
		if got := probeFailureText(c.kind, "raw"); !strings.Contains(got, c.want) {
			t.Errorf("probeFailureText(%v)=%q want 含 %q", c.kind, got, c.want)
		}
	}
}

// TestProbeModelsList 模型下拉框内容：来自模型页实时表，为空时给占位提示。
func TestProbeModelsList(t *testing.T) {
	if got := probeModelChoices(nil); len(got) != 1 || !strings.Contains(got[0], "模型") {
		t.Errorf("空表应给占位提示: %v", got)
	}
	got := probeModelChoices([]modelRateRow{{ID: "glm-5.2"}, {ID: "deepseek-v4.1-flash"}})
	if len(got) != 2 || got[0] != "glm-5.2" {
		t.Errorf("应返回模型 ID 列表: %v", got)
	}
}

// TestProbeAccountChoices 账号下拉框：昵称优先，禁用/冷却账号照样列出（用户有权测它们），
// 但要标注状态。
func TestProbeAccountChoices(t *testing.T) {
	// 该函数只依赖 svc，这里只测纯函数部分：状态标注
	if got := probeAccountLabel("木瓜", "4bfda6dd-1717", "正常"); !strings.Contains(got, "木瓜") {
		t.Errorf("应含昵称: %q", got)
	}
	if got := probeAccountLabel("", "00e26541abcdef", "正常"); !strings.Contains(got, "00e2654") {
		t.Errorf("无昵称应回落 UID 前 7 位: %q", got)
	}
}

// TestRunProbeNilService 防御：服务未初始化时 runProbe 必须返回结构完整的失败结果，
// 绝不能 panic（测试环境/异常装配下 a.svc 可能为 nil）。
func TestRunProbeNilService(t *testing.T) {
	a := &app{} // svc 为 nil
	res := runProbe(a, "u1", "glm-5.2", 0, "")
	if res == nil {
		t.Fatal("runProbe 必须返回非 nil")
	}
	if res.OK {
		t.Error("服务未运行不应成功")
	}
	if res.Detail == "" {
		t.Error("失败应有原因说明")
	}
	if s := res.Summary(); !strings.Contains(s, "失败") {
		t.Errorf("摘要=%q", s)
	}
}
