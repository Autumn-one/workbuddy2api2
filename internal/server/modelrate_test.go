package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// blockedBody 模拟 6004 报文。
const blockedBody = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2027-01-01 00:00:00 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"r"}`

// countingFakeUpstream 可统计上游调用次数的 fake（ChatStream 走 HTTP transport）。
type countingFakeUpstream struct {
	*upstream.Client
	mu    sync.Mutex
	calls int
}

func (c *countingFakeUpstream) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newCountingFakeUpstream(t *testing.T, behavior func(auth string) (int, string, bool)) *countingFakeUpstream {
	t.Helper()
	var c *countingFakeUpstream
	c = &countingFakeUpstream{Client: &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			c.mu.Lock()
			c.calls++
			c.mu.Unlock()
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}}
	return c
}

// httptestPost 便捷 POST /v1/chat/completions。
func httptestPost(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

// TestModelRateLimitCooldownAfter6004 端到端：撞 6004 后该（账号×模型）必须被冷却，
// 后续同模型请求不得再选中该账号（这是本次修复的核心诉求：不再反复撞墙）。
func TestModelRateLimitCooldownAfter6004(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, blockedBody, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	// 第一次：撞 6004（返回 503，轮换耗尽）
	rec := httptestPost(h, `{"model":"deepseek-v4.1-flash","messages":[]}`)
	if rec.Code != 503 {
		t.Fatalf("首次撞 6004 应 503，got %d body=%s", rec.Code, rec.Body)
	}
	if !p.IsModelCooling("u1", "deepseek-v4.1-flash") {
		t.Fatal("6004 后该（账号×模型）应进入冷却")
	}
	d := time.Until(p.ModelCooldownUntil("u1", "deepseek-v4.1-flash"))
	if d <= 0 || d > modelRateCooldownBase+time.Second {
		t.Fatalf("首次冷却应约 60s，got %v", d)
	}

	// 后续同模型请求：不得再打 u1（冷却中且无其他账号 → 直接 503，无上游调用）
	callsBefore := up.count()
	rec = httptestPost(h, `{"model":"deepseek-v4.1-flash","messages":[]}`)
	if rec.Code != 503 {
		t.Fatalf("冷却中同模型应 503，got %d", rec.Code)
	}
	if up.count() != callsBefore {
		t.Errorf("冷却中不应再发起上游调用（撞墙次数 %d → %d）", callsBefore, up.count())
	}
}

// TestModelRateLimitCooldownIsPerModel 冷却按模型隔离：
// 同账号的另一个模型不受影响，可正常选中并转发。
func TestModelRateLimitCooldownIsPerModel(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, blockedBody, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	// glm-5.3 撞 6004 → 冷却
	httptestPost(h, `{"model":"glm-5.3","messages":[]}`)
	if !p.IsModelCooling("u1", "glm-5.3") {
		t.Fatal("glm-5.3 应冷却")
	}
	// 换模型：upstream 仍会返回 6004（fake 不分模型），但这恰好证明【请求确实发出去了】
	// ——即冷却没有误伤其他模型（否则连上游都不会被调）。
	calls := up.count()
	httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if up.count() != calls+1 {
		t.Errorf("其他模型应照常发起上游调用（不受 glm-5.3 冷却影响）: %d → %d", calls, up.count())
	}
}

// TestModelRateLimitCooldownBackoffIntegration 集成：连续撞墙冷却翻倍。
func TestModelRateLimitCooldownBackoffIntegration(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, blockedBody, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	httptestPost(h, `{"model":"m","messages":[]}`)
	d1 := time.Until(p.ModelCooldownUntil("u1", "m"))
	// 手动把冷却拨到过去，模拟窗口自然结束
	p.AdvanceModelCooldowns(time.Now().Add(time.Minute))
	httptestPost(h, `{"model":"m","messages":[]}`)
	d2 := time.Until(p.ModelCooldownUntil("u1", "m"))
	if d1 <= 0 || d2 <= d1 {
		t.Fatalf("连续撞墙应翻倍: 第一次=%v 第二次=%v", d1, d2)
	}
	if d2 < 2*modelRateCooldownBase-time.Second {
		t.Fatalf("第二次应 ≥ 2 分钟: %v", d2)
	}
}

// TestModelRateLimitCooldownExpires 恢复：到期后同模型可正常选中。
func TestModelRateLimitCooldownExpires(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	p.NoteModelRateLimit("u1", "deepseek-v4.1-flash", modelRateCooldownBase)
	p.AdvanceModelCooldowns(time.Now().Add(time.Minute))

	rec := httptestPost(h, `{"model":"deepseek-v4.1-flash","messages":[]}`)
	if rec.Code != 200 {
		t.Fatalf("到期后应恢复正常转发，got %d body=%s", rec.Code, rec.Body)
	}
}

// TestModelRateLimitStickyUnbindsWhenCooled 粘性会话：粘住的账号对该模型冷却时，
// 必须解绑并回落其他账号（否则粘住一个限额号只会反复撞墙）。
func TestModelRateLimitStickyUnbindsWhenCooled(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	store := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     store,
		Available: func() []string { return []string{"u1", "u2"} },
	})
	h := NewHandler(Config{Pool: p, Upstream: up.Client, Session: sess, StickyCount: func() int { return 0 }})

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"sticky-k"}}`
	// 首次成功 → 绑定
	rec := httptestPost(h, body)
	if rec.Code != 200 {
		t.Fatalf("首次应成功: %d", rec.Code)
	}
	bound, ok := sess.Resolve("sticky-k")
	if !ok || bound == "" {
		t.Fatal("成功后应建立粘性绑定")
	}
	// 该账号对此模型进入冷却
	p.NoteModelRateLimit(bound, "deepseek-v4.1-flash", modelRateCooldownBase)
	calls := up.count()
	// 再请求：粘住的账号对该模型已冷却 → 必须解绑并回落【另一个】账号成功。
	rec2 := httptestPost(h, body)
	if rec2.Code != 200 {
		t.Fatalf("解绑后应回落其他账号成功: %d", rec2.Code)
	}
	if up.count() != calls+1 {
		t.Errorf("应恰好再发一次上游调用: %d → %d", calls, up.count())
	}
	// 粘性绑定应已切到另一个账号（解绑→成功号重绑）。
	rebound, ok := sess.Resolve("sticky-k")
	if !ok || rebound == "" {
		t.Fatal("成功后应重建粘性绑定")
	}
	if rebound == bound {
		t.Errorf("绑定仍在被冷却的账号 %s 上", bound)
	}
}

// TestChatLogAlwaysHasUID 每一行请求日志都必须带 uid=（哪怕失败/冷却）。
func TestChatLogAlwaysHasUID(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, blockedBody, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client})

	out := captureStdout(t, func() {
		rec := httptestPost(h, `{"model":"m","messages":[]}`)
		if rec.Code != 503 {
			t.Fatalf("got %d", rec.Code)
		}
	})
	if !strings.Contains(out, "uid=u1") {
		t.Errorf("6004 行必须带账号: %s", out)
	}
	if !strings.Contains(out, "effort=-") || !strings.Contains(out, "ctx=-") {
		t.Errorf("失败行也应带参数位: %s", out)
	}
}
