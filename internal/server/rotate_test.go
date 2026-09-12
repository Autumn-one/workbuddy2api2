package server

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ─────────────── 轮转次数可配 + 解析失败也轮转 ───────────────
//
// 生产事故（2026-09-13 02:57 / 03:07）：21 个账号的池子里，MaxRotate=3 用完后
// 返回 503，而 8 秒后就成功了——说明可用账号一直存在，只是 3 次抽样没抽到
// （池里有 5~6 个被 11140 标记或 6004 限流的"坏号"）。
//
// 两个改动：
//  1. max_rotate 提成配置项（默认从 3 提到 5）；
//  2. 非流式的 Aggregate 解析失败也参与轮转（此前直接 502，不换号）。

// TestMaxRotateConfigurable 配置的轮转次数必须生效。
func TestMaxRotateConfigurable(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"code":500,"msg":"boom"}`, false // 全部失败，逼出轮转上限
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u4", AccessToken: "at4", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u5", AccessToken: "at5", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u6", AccessToken: "at6", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.Client, MaxRotate: 5})

	rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("应 503, got %d", rec.Code)
	}
	if got := up.count(); got != 5 {
		t.Errorf("MaxRotate=5 应产生 5 次上游调用, got %d", got)
	}
}

// TestMaxRotateDefaultIsFive 默认值 5（原为 3——在 21 账号池里不够）。
func TestMaxRotateDefaultIsFive(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	if h.cfg.MaxRotate != 5 {
		t.Errorf("默认 MaxRotate=%d want 5", h.cfg.MaxRotate)
	}
}

// TestMaxRotateOneNoRotation 配 1 = 只试一次（等价"关闭轮转"，把错误直接暴露）。
func TestMaxRotateOneNoRotation(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.Client, MaxRotate: 1})
	httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if got := up.count(); got != 1 {
		t.Errorf("MaxRotate=1 应只调用 1 次（关闭轮转）, got %d", got)
	}
}

// TestAggregateFailureRotates 非流式的流解析失败必须换号重试（此前直接 502）。
//
// 场景：上游返回 200 + 畸形 SSE（无有效数据帧）→ Aggregate 报错。
// 此前行为：直接 502 返回客户端（不换号）。
// 期望行为：换号重试；若后续账号成功则应返回 200。
func TestAggregateFailureRotates(t *testing.T) {
	withChatLog(t)
	// 第一个账号返回畸形流，第二个返回正常流
	var n int
	up := newCountingFakeUpstream(t, func(authz string) (int, string, bool) {
		n++
		if strings.Contains(authz, "at1") {
			return 200, "data: {\"broken", false // 畸形：无有效数据帧
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.Client, MaxRotate: 3})

	rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("解析失败后应换号并成功, got %d body=%s", rec.Code, rec.Body)
	}
	if n < 2 {
		t.Errorf("应发生轮转（≥2 次上游调用）, got %d", n)
	}
}

// TestAggregateFailureAllBad 全部账号解析失败时返回 502（保留原有语义）。
func TestAggregateFailureAllBad(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"broken", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up.Client, MaxRotate: 3})
	rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("全部解析失败应 502, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream_parse") {
		t.Errorf("错误码应为 upstream_parse: %s", rec.Body)
	}
}

// TestAggregateFailureDoesNotDisableAccount 解析失败不得禁用账号
// （是流格式问题，账号本身可用；不应因网络抖动把账号永久拉黑）。
func TestAggregateFailureDoesNotDisableAccount(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"broken", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client, MaxRotate: 1})
	httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Error("解析失败不应禁用账号")
	}
	if upstream.ErrNone == upstream.ErrServer {
		t.Fatal("unreachable")
	}
}

// TestTransportErrorStillReturns503 回归：传输层错误必须返回 503（不是 502）。
//
// 缺陷背景：引入"解析失败也轮转"时，我用"不是 upstream.Error"作为解析失败的判据，
// 结果把传输错误（网络问题）也误判成解析失败（上游坏数据），返回了 502。
// 两者语义不同：503 = 等账号/网络恢复后重试；502 = 上游返回了无法解析的数据。
func TestTransportErrorStillReturns503(t *testing.T) {
	withChatLog(t)
	up := newCountingFakeUpstream(t, func(string) (int, string, bool) {
		return 0, "", false // 配合下方自定义 transport 触发传输错误
	})
	// 用返回 error 的 transport 制造真实传输错误
	up.Client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errStr("connection refused")
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up.Client, MaxRotate: 2})

	rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("传输错误应 503（可重试），got %d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "upstream_parse") {
		t.Errorf("传输错误不应报为 upstream_parse: %s", rec.Body)
	}
}

// TestParseAndTransportDistinguished 区分判据本身：解析失败 vs 传输错误。
func TestParseAndTransportDistinguished(t *testing.T) {
	if !isParseFailure(&parseFailure{err: errStr("bad stream")}) {
		t.Error("parseFailure 应被识别为解析失败")
	}
	if isParseFailure(errStr("connection refused")) {
		t.Error("普通传输错误不得被识别为解析失败")
	}
	if isParseFailure(nil) {
		t.Error("nil 不得被识别为解析失败")
	}
	if isParseFailure(&upstream.Error{Kind: upstream.ErrServer}) {
		t.Error("upstream.Error 是分类错误，不是解析失败")
	}
}

// errStr 简单 error 构造（避免引 fmt）。
type errStr string

func (e errStr) Error() string { return string(e) }
