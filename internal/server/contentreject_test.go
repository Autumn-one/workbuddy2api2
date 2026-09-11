package server

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ─────────────── 11140 内容拒绝 → 账号短期冷却 ───────────────
//
// 需求：连续撞 11140 的账号自动短期冷却，避免反复白撞。
//
// 为什么是"短期"而非"禁用"：生产实证（2026-09-12）显示 eedf4e88 两小时内
// 189 次全被拒，但风控标记通常是临时的；禁用会让账号永久退场，误伤风险大。
// 冷却到期自动恢复，并可在检测页手动复测。

// blocked11140 生产真实报文（403 + code 11140）。
const blocked11140 = `{"code":11140,"msg":"request illegal","requestId":"x","displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.","zh":"内容未通过安全审核，请调整后重试"}}`

// TestContentRejectCooldownApplied 撞 11140 后该账号必须进入冷却。
func TestContentRejectCooldownApplied(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 403, blocked11140, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != 503 {
		t.Fatalf("全部账号被拒时应 503, got %d", rec.Code)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.Cooling {
		t.Fatal("撞 11140 后该账号应进入冷却（避免反复白撞）")
	}
	if st.CoolKind != "soft_rate" {
		t.Errorf("应为软冷却, got cool_kind=%q", st.CoolKind)
	}
	// 冷却时长应短暂（时长常量），不应是"到次日 4 点"这类长冷却。
	remain := time.Until(st.Until)
	if remain <= 0 || remain > contentRejectCooldown+10*time.Second {
		t.Errorf("冷却剩余 %v 应约等于 %v（短期）", remain, contentRejectCooldown)
	}
}

// TestContentRejectCooldownSkipsAccount 冷却期内该账号不再被选中——
// 这是本次改动的核心收益：不再反复浪费上游往返。
func TestContentRejectCooldownSkipsAccount(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 403, blocked11140, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	// 第一次请求：轮换最多 3 次，两个账号都会被试到
	httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	callsAfterFirst := up.count()
	if callsAfterFirst == 0 {
		t.Fatal("首次请求应产生上游调用")
	}

	// 第二次请求：两个账号都在冷却中 → 不应再产生上游调用（快速失败）
	httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if got := up.count(); got != callsAfterFirst {
		t.Errorf("冷却中不应再打上游（白撞）: %d → %d", callsAfterFirst, got)
	}
}

// TestContentRejectNotCountedAsBreaker 11140 不得喂熔断计数：
// 账号本身是健康的（上游明确说"内容"问题），喂熔断会把账号错误地熔断掉。
func TestContentRejectNotCountedAsBreaker(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 403, blocked11140, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	for i := 0; i < 5; i++ {
		// 清掉冷却以便反复撞（模拟长时间观察）。
		// 注意：必须用 CooldownSoftOnly 重置——Cooldown 本身会喂熔断（它是失败信号），
		// 用它重置会把测试自身的行为混进来，导致断言失效。
		p.CooldownSoftOnly("u1", time.Millisecond, "test reset")
		time.Sleep(5 * time.Millisecond)
		httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	}
	st, _ := p.Status("u1")
	if st.BreakerFails != 0 {
		t.Errorf("11140 不应喂熔断计数, BreakerFails=%d", st.BreakerFails)
	}
	if !st.BreakerUntil.IsZero() {
		t.Errorf("11140 不应触发熔断, BreakerUntil=%v", st.BreakerUntil)
	}
	if st.Disabled {
		t.Error("11140 不应禁用账号（风控多为临时）")
	}
}

// TestContentRejectLogsClearly 11140 必须有专门日志（此前混在 403 client 里不显眼），
// 且要能看出"是哪个账号被拒"，便于定位被风控标记的账号。
func TestContentRejectLogsClearly(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 403, blocked11140, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "被风控的号", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	// log.Printf 写 stderr，captureStdout 只截 os.Stdout —— 必须改用 log 的输出目标。
	var buf bytes.Buffer
	oldW := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldW)

	httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	out := buf.String()
	if !strings.Contains(out, "content_rejected") {
		t.Errorf("日志应含 content_rejected 标识:\n%s", out)
	}
	if !strings.Contains(out, "被风控的号") || !strings.Contains(out, "glm-5.2") {
		t.Errorf("日志应含账号与模型，便于定位:\n%s", out)
	}
}

// TestContentRejectCooldownIsShort 冷却时长必须是"短期"且为正。
func TestContentRejectCooldownIsShort(t *testing.T) {
	if contentRejectCooldown <= 0 {
		t.Fatalf("contentRejectCooldown=%v 必须为正", contentRejectCooldown)
	}
	if contentRejectCooldown > 30*time.Minute {
		t.Errorf("contentRejectCooldown=%v 过长（风控多为临时，长冷却误伤风险大）", contentRejectCooldown)
	}
	t.Logf("11140 冷却时长 = %v", contentRejectCooldown)
}

// TestContentRejectStillRotatesToHealthyAccount 关键不回归：被风控账号冷却后，
// 请求仍能由健康账号正常服务（不得因冷却导致整体不可用）。
func TestContentRejectStillRotatesToHealthyAccount(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at-bad") {
			return 403, blocked11140, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("应轮换到健康账号并成功, got %d body=%s", rec.Code, rec.Body)
	}
	// 被风控账号进入冷却，健康账号不受影响
	badSt, _ := p.Status("bad")
	goodSt, _ := p.Status("good")
	if !badSt.Cooling {
		t.Error("被风控账号应冷却")
	}
	if goodSt.Cooling {
		t.Error("健康账号不应被连带冷却")
	}
}
