package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestContextOverflowResponseIsSDKReadable 守住【客户端真能读到】这条契约。
//
// 实测教训（2026-09-13 05:44）：先前"原样透传上游 body"的写法让 pi 只看到
// "400 status code (no body)" —— 因为 openai SDK 在非 2xx 时只读 body 的
// `error` 键（core/error.mjs: errorResponse?.['error']），上游自有形状
// {code,msg,extError,displayMsg} 没有该键，SDK 就把整个 body 丢掉了。
//
// 本测试直接按 SDK 的读取规则断言：必须存在 error.message，且其中含
// context_length_exceeded（pi 的溢出识别只做模式匹配）。
func TestContextOverflowResponseIsSDKReadable(t *testing.T) {
	const upstreamBody = `{"code":11115,"msg":"input length too long",` +
		`"extError":{"code":"context_length_exceeded","message":"input length too long","type":"invalid_request_error"},` +
		`"displayMsg":{"en":"The request exceeds the model context limit.","zh-hans":"对话内容超出模型长度上限，请精简对话或减少附件后重试。"}}`

	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 400, upstreamBody, false })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
	// SDK 要求 error.message 存在（否则报 "no body"）。
	if !strings.Contains(string(raw), `"error"`) || !strings.Contains(string(raw), `"message"`) {
		t.Fatalf("缺少 OpenAI 信封 error.message，SDK 会报 'no body':\n%s", raw)
	}
	// pi 的溢出识别靠这个模式串。
	if !strings.Contains(string(raw), "context_length_exceeded") {
		t.Errorf("error.message 必须含 context_length_exceeded（pi 靠它识别溢出）:\n%s", raw)
	}
	// 上游原始字段保留，便于诊断与其它客户端。
	for _, want := range []string{`"code":11115`, "displayMsg", "input length too long"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("上游字段 %q 丢失:\n%s", want, raw)
		}
	}
}
