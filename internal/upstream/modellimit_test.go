package upstream

import "testing"

// TestClassifyModelRateLimit 验证模型级频率限制被正确识别，且【不会】被误判为余额不足。
// 这条修复的核心价值：该文案此前在 200 状态被当成功、400 状态只换号不冷却、
// 429 状态只冷却 60s —— 三种情况都无法被观测到。
func TestClassifyModelRateLimit(t *testing.T) {
	// 用户实测原文（WorkBuddy 客户端）
	const realMsg = "当前您在Deepseek-V4.1-Flash模型的使用量已超出频率限制，可在2026-09-11 22:56:11 重置可用。您可切换其他模型或消耗积分继续使用该模型"

	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"实测原文+200", 200, `{"code":10001,"msg":"` + realMsg + `"}`, ErrModelRateLimit},
		{"实测原文+400", 400, `{"code":10001,"msg":"` + realMsg + `"}`, ErrModelRateLimit},
		{"实测原文+429", 429, `{"code":10001,"msg":"` + realMsg + `"}`, ErrModelRateLimit},
		{"裸文案", 200, realMsg, ErrModelRateLimit},
		{"英文变体", 200, `{"msg":"model usage limit exceeded, please switch to another model"}`, ErrModelRateLimit},

		// 关键回归：不能把纯余额问题误吸成模型限流
		{"余额不足仍走hard", 400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{"402仍走hard", 402, ``, ErrHardCredit},

		// 关键回归：普通 429（无模型语境）仍走 soft_rate，不受影响
		{"普通429", 429, `{"msg":"too many requests"}`, ErrSoftRate},
		{"空body+429", 429, ``, ErrSoftRate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Errorf("Classify(%d) = %v (%s), want %v", c.status, got, got.String(), c.want)
			}
		})
	}
}

// TestParseModelRateLimitFromMsg 验证证据提取只在该类限制时成功。
func TestParseModelRateLimitFromMsg(t *testing.T) {
	const realMsg = "当前您在Deepseek-V4.1-Flash模型的使用量已超出频率限制，可在2026-09-11 22:56:11 重置可用。"

	ev, ok := ParseModelRateLimitFromMsg(200, realMsg)
	if !ok {
		t.Fatal("should recognize model rate limit")
	}
	if ev.Status != 200 || ev.Msg == "" {
		t.Errorf("evidence = %+v", ev)
	}
	// 必须保留原文供观测（截断上限 300）
	if len(ev.Msg) > 300 {
		t.Errorf("msg too long: %d", len(ev.Msg))
	}

	if _, ok := ParseModelRateLimitFromMsg(400, "余额不足"); ok {
		t.Error("余额不足 must not be recognized as model rate limit")
	}
	if _, ok := ParseModelRateLimitFromMsg(429, "too many requests"); ok {
		t.Error("plain 429 must not be recognized as model rate limit")
	}
}

// TestParseCreditsRate 验证模型倍率解析。
// 实测上游取值： "x0.51 credits" / "x0.03" / "x2.20 credits" / "" (auto 等无倍率)
func TestParseCreditsRate(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"x0.51 credits", 0.51, true},
		{"x0.03", 0.03, true},
		{"x2.20 credits", 2.20, true},
		{"x1.62 credits", 1.62, true},
		{"x0.00", 0, true},
		{"", 0, false},
		{"credits", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseCreditsRate(c.in)
		if ok != c.ok {
			t.Errorf("ParseCreditsRate(%q) ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParseCreditsRate(%q)=%v want %v", c.in, got, c.want)
		}
	}
}
