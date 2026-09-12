package upstream

import (
	"strings"
	"testing"
)

// ─────────────── 签到结果判定（修正误判）───────────────
//
// 生产实测缺陷（2026-09-13，代理不支持时暴露）：
// 代理端口拒绝连接 → DailyCheckin 返回网络层错误，但错误串里含
// URL 路径 "/v2/billing/meter/daily-checkin"，而 IsAlreadyCheckedIn 匹配子串
// "checkin" → 被误判为「今天已签到」，签到结果显示为成功。
//
// 后果：真实的签到失败被静默伪装成"已签到"，用户与统计都被误导
// （实测日志里 21 个账号全显示"今天已签到"，其中部分是代理失败）。
//
// 修正原则：只认【上游业务报文】里的"已签到"特征，不认 URL/函数名里的 checkin。

// TestIsAlreadyCheckedInRealUpstreamBody 真实上游报文（重复签到）应判为已签到。
func TestIsAlreadyCheckedInRealUpstreamBody(t *testing.T) {
	// 上游对重复签到的真实响应（code=10001）
	body := `upstream client (http 400): {"code":10001,"msg":"今天已签到，请明天再来","requestId":"x"}`
	if !IsAlreadyCheckedIn(body) {
		t.Errorf("真实重复签到报文应判为已签到: %s", body)
	}
}

// TestIsAlreadyCheckedInRejectsNetworkError 网络层错误不得判为已签到。
func TestIsAlreadyCheckedInRejectsNetworkError(t *testing.T) {
	cases := []string{
		// 代理连接失败（生产实测原文）：含 checkin 路径但不含业务报文
		`upstream client (http 0): Post "https://www.codebuddy.cn/v2/billing/meter/daily-checkin": proxyconnect tcp: dial tcp 127.0.0.1:34567: connectex: No connection could be made because the target machine actively refused it.`,
		// 超时
		`upstream client (http 0): Post "https://www.codebuddy.cn/v2/billing/meter/daily-checkin": context deadline exceeded`,
		// DNS 失败
		`Post "https://www.codebuddy.cn/v2/billing/meter/daily-checkin": dial tcp: lookup www.codebuddy.cn: no such host`,
		// 上游 500（不是"已签到"）
		`upstream client (http 500): {"code":500,"msg":"internal error"}`,
		// session 失效
		`upstream client (http 401): {"code":12153,"msg":"Offline user session not found"}`,
	}
	for _, c := range cases {
		if IsAlreadyCheckedIn(c) {
			t.Errorf("非业务错误不得判为已签到:\n%s", c)
		}
	}
}

// TestIsAlreadyCheckedInOnlyBusinessMessages 只有含业务码/文案的报文才算。
func TestIsAlreadyCheckedInOnlyBusinessMessages(t *testing.T) {
	yes := []string{
		`{"code":10001,"msg":"今天已签到，请明天再来"}`,
		`{"code":10001,"msg":"already checked in today"}`,
		`upstream client (http 400): {"code":10001,"msg":"今天已签到"}`,
	}
	for _, c := range yes {
		if !IsAlreadyCheckedIn(c) {
			t.Errorf("业务报文应判为已签到:\n%s", c)
		}
	}
}

// TestIsAlreadyCheckedInCaseInsensitive 英文文案大小写不敏感。
func TestIsAlreadyCheckedInCaseInsensitive(t *testing.T) {
	if !IsAlreadyCheckedIn(`{"msg":"ALREADY Checked In"}`) {
		t.Error("英文文案应大小写不敏感")
	}
}

// TestIsAlreadyCheckedInEmpty 空串/纯 URL 不算（防止把路径当判据）。
func TestIsAlreadyCheckedInEmpty(t *testing.T) {
	for _, c := range []string{"", "   ", "/v2/billing/meter/daily-checkin"} {
		if IsAlreadyCheckedIn(c) {
			t.Errorf("%q 不应判为已签到", c)
		}
	}
}

// TestIsAlreadyCheckedInNoCheckinSubstring 关键回归：不得因 URL 含 checkin 而误判。
func TestIsAlreadyCheckedInNoCheckinSubstring(t *testing.T) {
	s := `Post "https://x/v2/billing/meter/daily-checkin": some failure`
	if strings.Contains(strings.ToLower(s), "checkin") && IsAlreadyCheckedIn(s) {
		t.Error("不得仅因 URL 含 checkin 子串就判为已签到（生产缺陷根因）")
	}
}
