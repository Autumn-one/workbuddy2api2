package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// captureStdout 重定向 os.Stdout 并捕获 fn 期间的全部输出。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	_ = w.Close()
	raw, _ := io.ReadAll(r)
	return string(raw)
}

// withChatLog 临时开启聊天表格日志（TestMain 默认关闭），测试结束后恢复。
// 仅供断言表格行输出的用例使用。
func withChatLog(t *testing.T) {
	t.Helper()
	old := chatLogEnabled
	chatLogEnabled = true
	t.Cleanup(func() { chatLogEnabled = old })
}

func TestChatStatsReaderTokensFromUsage(t *testing.T) {
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Fatalf("tokens=%d ok=%v, want 1/true (from usage, not rune count)", toks, ok)
	}
	// TTFB 用 time.Since 计时，精度到纳秒；但 sseOK 是内存 reader，
	// 首帧解析可能与 start 落在同一时钟刻度内（Windows 粒度 ~0.5-15.6ms），
	// 此时 time.Since(start)==0 是【物理正确】的——不是缺陷。
	// 原断言写死 >0，在快速机器上必然偶发失败（flaky）。
	//
	// 这里改断言真正要保证的性质：
	//  1) 见过 data 帧 → seen 置位（TTFB 记录逻辑被触发）
	//  2) TTFB 非负（不会出现负值/时钟回拨污染）
	if r.TTFB() < 0 {
		t.Errorf("ttfb=%v must not be negative", r.TTFB())
	}
	if !r.seen {
		t.Error("见到 data 帧后 seen 应置位（TTFB 记录逻辑未触发）")
	}
}

func TestChatStatsReaderNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); ok || toks != 0 {
		t.Errorf("tokens=%d ok=%v, want 0/false for missing usage", toks, ok)
	}
}

func TestChatStatsReaderLastFrameUsageWins(t *testing.T) {
	sse := "data: {\"usage\":{\"completion_tokens\":5}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	toks, ok := r.Tokens()
	if !ok || toks != 12 {
		t.Fatalf("tokens=%d ok=%v, want 12 (last frame wins)", toks, ok)
	}
}

func TestChatStatsReaderTTFBOnlyOnDataFrame(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	var s chatStatsReader
	s.start = start
	s.parseSSELine("event: ping")
	if s.TTFB() != 0 {
		t.Errorf("non-data line must not set TTFB: %v", s.TTFB())
	}
	s.parseSSELine("data: {\"choices\":[]}")
	first := s.TTFB()
	if first < time.Second {
		t.Errorf("first data frame TTFB=%v want >=2s", first)
	}
	s.parseSSELine("data: {\"choices\":[]}")
	if s.TTFB() != first {
		t.Errorf("second frame changed TTFB: %v -> %v", first, s.TTFB())
	}
}

func TestChatStatsReaderBytesPassthrough(t *testing.T) {
	sse := "data: {\"content\":\"你好\"}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	out, _ := io.ReadAll(r)
	if string(out) != sse {
		t.Errorf("passthrough mismatch:\n got %q\nwant %q", out, sse)
	}
}

func TestParseModelFromBody(t *testing.T) {
	if got := parseModelFromBody([]byte(`{"model":"deepseek-v4-flash","stream":true}`)); got != "deepseek-v4-flash" {
		t.Errorf("got %q", got)
	}
	if got := parseModelFromBody([]byte(`{}`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
	if got := parseModelFromBody([]byte(`not json`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
}

func TestCompletionTokensExtraction(t *testing.T) {
	got := completionTokens(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 234.0, "total_tokens": 244.0},
	})
	if got != 234 {
		t.Errorf("got %d want 234", got)
	}
	if got := completionTokens(map[string]any{}); got != -1 {
		t.Errorf("missing usage: got %d want -1", got)
	}
	if got := completionTokens(map[string]any{"usage": map[string]any{}}); got != -1 {
		t.Errorf("missing completion_tokens: got %d want -1", got)
	}
}

func TestLogAccountNameFallback(t *testing.T) {
	// 无昵称时回落 UID 前 8 位（旧 uidPrefix 的语义在回落分支里保留）
	if got := logAccountName(&auth.Auth{UID: "00e26541abcdef012345"}); got != "00e26541" {
		t.Errorf("long uid -> %q", got)
	}
	if got := logAccountName(&auth.Auth{UID: "abc"}); got != "abc" {
		t.Errorf("short uid -> %q", got)
	}
	if got := logAccountName(&auth.Auth{}); got != "-" {
		t.Errorf("empty uid -> %q", got)
	}
	if got := logAccountName(nil); got != "-" {
		t.Errorf("nil acct -> %q", got)
	}
}

func TestLogChatRowFormat(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(412*time.Millisecond, 27100*time.Millisecond, "deepseek-v4-flash", "stream", &auth.Auth{UID: "00e26541abcdef"}, http.StatusOK, 1234, 5000, 1200, 1800, upstream.EffectiveParams{Effort: "high", EffortReq: "high", MaxTokens: 8192})
	})
	for _, want := range []string{
		"| #", "deepseek-v4", "| stream |", "| 200 |", "00e26541", "effort=high", "max=8192", "ctx=5000", "TTFB=412ms", "tok=1234", "tok/s |", "total=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00e26541abcdef") {
		t.Errorf("full uid leaked: %s", out)
	}
}

func TestLogChatRowNoUsageShowsDash(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync", &auth.Auth{UID: "s1"}, http.StatusServiceUnavailable, -1, -1, -1, -1, upstream.EffectiveParams{})
	})
	for _, want := range []string{"effort=-", "max=-", "thinkctl=-", "ctx=-", "TTFB=-", "tok=-", "-tok/s", "| 503 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
}

// TestLogChatRowThinkCtl thinkctl 列要原样展示客户端的思考开关指令
//（与 think= 列的实际思考 token 数配合，核对"关思考"是否生效）。
func TestLogChatRowThinkCtl(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", &auth.Auth{UID: "u"}, 200, 1, -1, -1, 0, upstream.EffectiveParams{ThinkCtl: "thinking:disabled"})
	})
	if !strings.Contains(out, "thinkctl=thinking:disabled") {
		t.Errorf("row missing thinkctl: %s", out)
	}
}

func TestLogChatRowSeqIncrements(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", &auth.Auth{UID: "u"}, 200, 1, -1, -1, -1, upstream.EffectiveParams{})
		logChatRow(0, time.Second, "m", "sync", &auth.Auth{UID: "u"}, 200, 1, -1, -1, -1, upstream.EffectiveParams{})
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	first := strings.Fields(lines[0])[1]
	second := strings.Fields(lines[1])[1]
	if !strings.HasPrefix(first, "#") || !strings.HasPrefix(second, "#") {
		t.Fatalf("seq columns missing: %q %q", first, second)
	}
	if first == second {
		t.Errorf("seq not incremented: %q == %q", first, second)
	}
}

func TestChatLogsStreamRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| stream |", "| 200 |", "acct=u1", "TTFB=", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream row missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "tok=1") {
		t.Errorf("tok: want precise usage completion_tokens: %s", out)
	}
}

func TestChatLogsSyncRowTTFBDash(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| sync |", "| 200 |", "TTFB=-", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync row missing %q:\n%s", want, out)
		}
	}
}

func TestChatLogsErrorRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	for _, want := range []string{"acct=u1", "| 503 |", "tok=-"} {
		if !strings.Contains(out, want) {
			t.Errorf("error row missing %q:\n%s", want, out)
		}
	}
}

func TestHealthzDoesNotLogTableRow(t *testing.T) {
	withChatLog(t) // 日志开启也应无表格行：非 chat 路由根本不走 logChatRow
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
		rec3 := httptest.NewRecorder()
		h.ServeHTTP(rec3, httptest.NewRequest("GET", "/status", nil))
	})
	if strings.Contains(out, "| #") {
		t.Errorf("healthz/models/status must not emit table rows:\n%s", out)
	}
}

// TestChatLogsParamsEndToEnd 端到端：请求日志行必须带出客户端指定的关键参数
// （思考深度 + 输出上限），否则用户无法从日志判断"这次请求到底用了什么配置"。
func TestChatLogsParamsEndToEnd(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-v4.1-flash","stream":true,"reasoning_effort":"max","max_tokens":4096,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"effort=max", "max=4096", "ctx=1", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("请求行缺少参数 %q:\n%s", want, out)
		}
	}
}

// TestChatLogsMissingParamsShowDash 未指定参数时显示 "-"（区分"没传"与"传了 0"），
// 且不影响该行其它字段。
func TestChatLogsMissingParamsShowDash(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"effort=-", "max=-", "| 200 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("请求行缺少 %q:\n%s", want, out)
		}
	}
}

// TestChatLogsDowngradedEffortShowsArrow 档位被降级时，日志须显示"实际←请求"，
// 让人一眼看出网关改写过请求（而不是误以为日志与客户端请求不符）。
//
// 通过 FetchModels 走真实的请求体降级通道填缓存（它才是生产路径），
// 不引入测试专用后门。
func TestChatLogsDowngradedEffortShowsArrow(t *testing.T) {
	withChatLog(t)
	modelsJSON := `{"code":0,"data":{"agents":[{"name":"cli","models":["hy4-preview"]}],"models":[` +
		`{"id":"hy4-preview","name":"HY4 Preview","maxInputTokens":100000,` +
		`"reasoning":{"supportedEfforts":["high"]}}]}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	// FetchModels 与 ChatStream 共用 HTTP transport；按 URL 路径分流来区分两者。
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/models") {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(modelsJSON)),
			}, nil
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 先拉一次模型表，把 supportedEfforts 灌进缓存（与启动时 loadModelRates 同路径）。
	if _, err := up.FetchModels(&auth.Auth{UID: "u1", AccessToken: "at1"}); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"hy4-preview","reasoning_effort":"max","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	if !strings.Contains(out, "effort=high←max") {
		t.Errorf("降级未被标注（期望 effort=high←max）:\n%s", out)
	}
}

// TestChatStatsReaderThinkingTokens 思考 token 必须从 usage 里如实读出。
//
// 它是本次要新增的关键参数之一：effort= 只是"请求了什么档位"，
// think= 才是"这个档位实际产生了多少思考"——后者才能验证档位是否真的生效。
// 实测（deepseek-v4.1-flash）上游同时给两个同值字段，此处两个都要认。
func TestChatStatsReaderThinkingTokens(t *testing.T) {
	cases := []struct {
		name string
		sse  string
		want int
	}{
		{
			name: "standard completion_tokens_details.reasoning_tokens",
			sse: "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":16," +
				"\"completion_tokens_details\":{\"reasoning_tokens\":16}}}\n\ndata: [DONE]\n\n",
			want: 16,
		},
		{
			name: "upstream completion_thinking_tokens fallback",
			sse:  "data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"completion_thinking_tokens\":7}}\n\ndata: [DONE]\n\n",
			want: 7,
		},
		{
			name: "thinking 0 is a real value not missing",
			sse:  "data: {\"usage\":{\"completion_tokens\":4,\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\ndata: [DONE]\n\n",
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newChatStatsReaderSince(strings.NewReader(c.sse), time.Now())
			_, _ = io.Copy(io.Discard, r)
			if got := r.ThinkingTokens(); got != c.want {
				t.Errorf("ThinkingTokens()=%d want %d", got, c.want)
			}
		})
	}
}

// TestChatStatsReaderThinkingTokensMissing 无思考字段时报告 -1（缺失），
// 与"思考为 0"区分开——否则日志无法区分"关闭思考"与"上游没报"。
func TestChatStatsReaderThinkingTokensMissing(t *testing.T) {
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if got := r.ThinkingTokens(); got != -1 {
		t.Errorf("ThinkingTokens()=%d want -1（缺失）", got)
	}
}

// TestChatStatsReaderPromptTokens 上下文（输入）用量必须来自 usage.prompt_tokens。
func TestChatStatsReaderPromptTokens(t *testing.T) {
	sse := "data: {\"usage\":{\"prompt_tokens\":1234,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if got := r.PromptTokens(); got != 1234 {
		t.Errorf("PromptTokens()=%d want 1234", got)
	}

	// 缺失 → -1
	r2 := newChatStatsReaderSince(strings.NewReader("data: {\"usage\":{\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"), time.Now())
	_, _ = io.Copy(io.Discard, r2)
	if got := r2.PromptTokens(); got != -1 {
		t.Errorf("缺失时 PromptTokens()=%d want -1", got)
	}
}

// TestChatLogsSyncRowIncludesPromptAndThinkingTokens 非流式路径同样要带出
// 上下文与思考用量（usage 走 Aggregate，字段口径必须与流式一致）。
func TestChatLogsSyncRowIncludesPromptAndThinkingTokens(t *testing.T) {
	withChatLog(t)
	sse := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
		"\"usage\":{\"prompt_tokens\":321,\"completion_tokens\":12," +
		"\"completion_tokens_details\":{\"reasoning_tokens\":9}}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sse, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| sync |", "ctx=321", "think=9", "tok=12"} {
		if !strings.Contains(out, want) {
			t.Errorf("非流式行缺少 %q:\n%s", want, out)
		}
	}
}

// TestPickCached 缓存读取 token 的取值规则：按优先级取第一个非零值。
//
// 回归点（实测）：上游把真值放在嵌套 prompt_tokens_details.cached_tokens，
// 顶层同名字段被填 0；旧实现只读顶层，于是 2026-10-01 的 2802 行请求日志
// cache= 全为 0，而同一批请求在下游（pi-ai 同样的取值顺序）读到 1.17B。
func TestPickCached(t *testing.T) {
	ptr := func(v int) *int { return &v }
	cases := []struct {
		name        string
		values      []*int
		want        int
		wantPresent bool
	}{
		{"嵌套真值 + 顶层影子 0", []*int{ptr(313344), nil, ptr(0), ptr(0)}, 313344, true},
		{"DeepSeek 自有字段", []*int{nil, ptr(1280), nil, nil}, 1280, true},
		{"顶层同名字段（保持旧口径）", []*int{nil, nil, ptr(800), ptr(800)}, 800, true},
		{"全 0 是合法值而非缺失", []*int{ptr(0), nil, ptr(0), nil}, 0, true},
		{"一个都不存在 = 缺失", []*int{nil, nil, nil, nil}, 0, false},
		{"高优先级的 0 不遮蔽低优先级真值", []*int{ptr(0), ptr(42)}, 42, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, present := pickCached(c.values...)
			if got != c.want || present != c.wantPresent {
				t.Errorf("pickCached()=(%d,%v) want (%d,%v)", got, present, c.want, c.wantPresent)
			}
		})
	}
}

// TestChatStatsReaderCachedTokens 流式末帧的缓存读取 token：
// 真值可能只在嵌套位置或 DeepSeek 自有字段，不能只认顶层。
func TestChatStatsReaderCachedTokens(t *testing.T) {
	cases := []struct {
		name string
		sse  string
		want int
	}{
		{
			name: "OpenAI 标准嵌套位置（顶层是 0 影子字段）",
			sse: "data: {\"usage\":{\"prompt_tokens\":313986,\"completion_tokens\":263," +
				"\"cached_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":313344}}}\n\ndata: [DONE]\n\n",
			want: 313344,
		},
		{
			name: "DeepSeek 自有字段 prompt_cache_hit_tokens",
			sse:  "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_cache_hit_tokens\":64}}\n\ndata: [DONE]\n\n",
			want: 64,
		},
		{
			name: "顶层 cache_read_input_tokens（无嵌套时）",
			sse:  "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"cache_read_input_tokens\":32}}\n\ndata: [DONE]\n\n",
			want: 32,
		},
		{
			name: "全 0 保留 0，不误报缺失",
			sse:  "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"cached_tokens\":0}}\n\ndata: [DONE]\n\n",
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newChatStatsReaderSince(strings.NewReader(c.sse), time.Now())
			_, _ = io.Copy(io.Discard, r)
			if got := r.CachedTokens(); got != c.want {
				t.Errorf("CachedTokens()=%d want %d", got, c.want)
			}
		})
	}
	// 完全无缓存字段 → -1（缺失），与"存在且为 0"区分
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if got := r.CachedTokens(); got != -1 {
		t.Errorf("无缓存字段时 CachedTokens()=%d want -1", got)
	}
}

// TestCachedTokensExtraction 非流式（Aggregate）路径的口径必须与流式一致。
func TestCachedTokensExtraction(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want int
	}{
		{
			name: "嵌套真值 + 顶层影子 0",
			resp: map[string]any{"usage": map[string]any{
				"prompt_tokens": 313986.0, "completion_tokens": 263.0,
				"cached_tokens":         0.0,
				"prompt_tokens_details": map[string]any{"cached_tokens": 313344.0},
			}},
			want: 313344,
		},
		{
			name: "DeepSeek 自有字段",
			resp: map[string]any{"usage": map[string]any{"prompt_cache_hit_tokens": 64.0}},
			want: 64,
		},
		{
			name: "顶层 cache_read_input_tokens",
			resp: map[string]any{"usage": map[string]any{"cache_read_input_tokens": 32.0}},
			want: 32,
		},
		{
			name: "存在但为 0 → 0（不是缺失）",
			resp: map[string]any{"usage": map[string]any{"cached_tokens": 0.0}},
			want: 0,
		},
		{
			name: "无缓存字段 → -1",
			resp: map[string]any{"usage": map[string]any{"prompt_tokens": 10.0}},
			want: -1,
		},
		{name: "无 usage → -1", resp: map[string]any{}, want: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cachedTokens(c.resp); got != c.want {
				t.Errorf("cachedTokens()=%d want %d", got, c.want)
			}
		})
	}
}
