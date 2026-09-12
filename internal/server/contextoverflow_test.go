package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestChatContextOverflowDoesNotRotate 守住上下文超限（11115）的处置：
// 这是【请求体自身尺寸】决定的确定性失败，与账号无关——必须
//  1. 不换号（上游只被调用 1 次，而不是 MaxRotate 次）；
//  2. 不罚账号（池里所有账号仍然可用，没有冷却）；
//  3. 原样透传上游 body（含 extError.code 与 displayMsg 多语言文案）。
//
// 第 3 条是恢复能力的来源：pi 靠 extError.code=context_length_exceeded（或文案特征）
// 识别"上下文超限"，才能触发自动压缩并重试。此前网关把它拼成 503
// "all accounts unavailable" 的 message，客户端只能看到一个无望的错误。
func TestChatContextOverflowDoesNotRotate(t *testing.T) {
	const upstreamBody = `{"code":11115,"msg":"input length too long","requestId":"65150a4c-a3a4-4a6b-b28d-72b4b05eb9e5",` +
		`"extError":{"code":"context_length_exceeded","message":"input length too long","type":"invalid_request_error"},` +
		`"displayMsg":{"zh-hans":"对话内容超出模型长度上限，请精简对话或减少附件后重试。","zh-hant":"對話內容超出模型長度上限，請精簡對話或減少附件後重試。"}}`

	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, upstreamBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 1) 不换号：只打一次上游（旧行为会打 5 次，每次白等约 6 秒）。
	if calls != 1 {
		t.Errorf("上游被调用 %d 次，want 1（上下文超限不得轮转）", calls)
	}
	// 2) 状态码保留上游的 400（旧行为返回 503 no_healthy_account）。
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s want 400", rec.Code, rec.Body)
	}
	// 3) 响应必须同时满足两件事：带 OpenAI 信封（SDK 才读得到，见
	//    core/error.mjs）且保留上游原始字段（诊断与其它客户端用）。
	//
	// 实测教训：只原样透传上游 body 时，openai SDK 因找不到 `error` 键而把
	// 整个 body 丢掉，pi 只看到 "400 status code (no body)" —— 连
	// context_length_exceeded 都看不到，自动压缩永远不会触发。
	got := rec.Body.String()
	for _, want := range []string{
		`"error"`,                 // SDK 读取入口
		"context_length_exceeded", // pi 的溢出识别模式串
		`"code":11115`,            // 上游原始字段
		"displayMsg",              // 上游多语言文案
		"对话内容超出模型长度上限",            // 面向人的中文说明
	} {
		if !strings.Contains(got, want) {
			t.Errorf("响应缺少 %q:\n%s", want, got)
		}
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if env["code"] != float64(11115) {
		t.Errorf("code=%v want 11115", env["code"])
	}
	ext, _ := env["extError"].(map[string]any)
	if ext == nil || ext["code"] != "context_length_exceeded" {
		t.Errorf("extError.code 丢失: %v —— 客户端靠它识别溢出并压缩恢复", env["extError"])
	}
	// 4) 不罚账号：池里 3 个号都应仍然健康（既没冷却也没熔断）。
	for _, uid := range []string{"u1", "u2", "u3"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("账号 %s 不在池中", uid)
		}
		if st.Cooling {
			t.Errorf("账号 %s 被冷却（%s）——上下文超限不是账号故障，不该罚", uid, st.Reason)
		}
	}
	total, healthy, cooling, _, _ := p.CountsDetailed()
	if total != 3 || healthy != 3 || cooling != 0 {
		t.Errorf("池状态 total=%d healthy=%d cooling=%d, want 3/3/0", total, healthy, cooling)
	}
}

// TestChatContextOverflowFreesInFlightLease 守住租约释放：
// 早期 return 路径必须把在途名额还回去，否则该账号名额会被这个请求一直占着
// （max_in_flight 默认 3，占满后新请求会被判 503）。
func TestChatContextOverflowFreesInFlightLease(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11115,"msg":"input length too long"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up})
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次请求 status=%d body=%s（租约未释放 → 名额被占死）", i+1, rec.Code, rec.Body)
		}
	}
}

// TestChatContextOverflowAfterRotationStillReported 守住混合序列：
// 先撞别的错误（需要换号）再撞上下文超限时，仍应把超限报文透传给客户端，
// 而不是继续轮转或退化成 503。
func TestChatContextOverflowAfterRotationStillReported(t *testing.T) {
	const oflowBody = `{"code":11115,"msg":"input length too long","extError":{"code":"context_length_exceeded"}}`
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 403, `{"code":11140,"msg":"request illegal"}`, false // 内容拒绝 → 换号
		}
		return 400, oflowBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))

	if calls != 2 {
		t.Errorf("上游调用 %d 次，want 2（第一号内容拒绝换号，第二号超限即停）", calls)
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "context_length_exceeded") {
		t.Errorf("status=%d body=%s want 400 且含 context_length_exceeded", rec.Code, rec.Body)
	}
}

// TestContextOverflowEndToEndOverSocket 真实套接字端到端验证（非 Recorder 内存直调）。
//
// 与上面各测试的区别：这里起真实 TCP 监听（httptest.NewServer）+ 真实 http.Client
// 发起请求，走完整的 HTTP 服务端栈。假上游仍是进程内 fake（不触真实上游、不消耗积分），
// 保证"修复在真实网络路径上生效"，而不只是内存调用成立。
func TestContextOverflowEndToEndOverSocket(t *testing.T) {
	const upstreamBody = `{"code":11115,"msg":"input length too long",` +
		`"extError":{"code":"context_length_exceeded","type":"invalid_request_error"},` +
		`"displayMsg":{"zh-hans":"对话内容超出模型长度上限，请精简对话或减少附件后重试。"}}`

	var calls int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(&calls, 1)
		return 400, upstreamBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 5, SoftCooldown: time.Minute})
	srv := httptest.NewServer(h)
	defer srv.Close()

	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("上游被调用 %d 次，want 1（真实套接字路径同样不得轮转）", n)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d want 400", resp.StatusCode)
	}
	if !strings.Contains(string(got), "context_length_exceeded") ||
		!strings.Contains(string(got), "对话内容超出模型长度上限") {
		t.Errorf("响应体未带出关键线索: %.300s", got)
	}
	// 旧行为：5 次轮转 × 每次约 6 秒。此处应远低于此（真实网络 + 本地 fake 应 < 1s）。
	if elapsed > 3*time.Second {
		t.Errorf("耗时 %v 过长——疑似仍在轮转", elapsed)
	}
}

// TestNonOverflowErrorKeepsOldEnvelope 回归：非超限错误（换号耗尽 → 503）仍走
// 原有 OpenAI 错误格式，未被本次"超限单独处理"的改动影响。
func TestNonOverflowErrorKeepsOldEnvelope(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("非超限错误仍应是 no_healthy_account 信封:\n%s", rec.Body)
	}
}
