package upstream

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestClassifyContextOverflow 验证上下文超限（11115）被正确识别为独立类别。
//
// 这条修复的核心价值：此前该错误落到 ErrClient → handler 走「只换号不罚」，
// 于是同一个超长请求体连撞 5 个账号、每次白耗约 6 秒（实测 11 个请求 × 5 次 = 50 次
// 上游 11115，合计每次 19.5~37.2 秒），最终仍是 503——因为请求体自身超限，
// 换号在原理上不可能成功。
func TestClassifyContextOverflow(t *testing.T) {
	// 用户实测原文（WorkBuddy 上游返回体的截断形态，与网关日志里的一模一样）。
	const realBody = `{"code":11115,"msg":"input length too long","requestId":"65150a4c-a3a4-4a6b-b28d-72b4b05eb9e5",` +
		`"extError":{"code":"context_length_exceeded","message":"input length too long","param":"","type":"invalid`

	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"实测原文+400", 400, realBody, ErrContextOverflow},
		{"实测原文+200", 200, realBody, ErrContextOverflow},
		{"完整报文", 400, `{"code":11115,"msg":"input length too long","extError":{"code":"context_length_exceeded","type":"invalid_request_error"}}`, ErrContextOverflow},
		{"仅 extError 英文码", 400, `{"extError":{"code":"context_length_exceeded"}}`, ErrContextOverflow},
		{"仅 msg 文案", 400, `{"msg":"input length too long"}`, ErrContextOverflow},
		{"裸文案", 400, "input length too long", ErrContextOverflow},
		{"code=11115 带空格", 400, `{"code": 11115}`, ErrContextOverflow},

		// 关键回归：不能把 1114 / 11140 当成 11115（子串与前缀都必须不误命中）。
		{"1114 不得误命中", 400, `{"code":1114}`, ErrClient},
		{"11140 仍走内容拒绝", 403, `{"code":11140,"msg":"request illegal"}`, ErrContentRejected},

		// 关键回归：其他 400 继续走 ErrClient（换号），不得被本类别吞掉。
		{"role 白名单 11128 仍走 client", 400, `{"code":11128,"msg":"invalid role"}`, ErrClient},
		{"普通 400", 400, `{"code":1,"msg":"bad request"}`, ErrClient},
		{"空 body+400", 400, ``, ErrClient},

		// 关键回归：429 限流不得被误判为上下文超限（那会放弃本该发生的换号/退避）。
		{"普通 429", 429, `{"msg":"too many requests"}`, ErrSoftRate},
		{"429+空 body", 429, ``, ErrSoftRate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body); got != c.want {
				t.Errorf("Classify(%d, %.60q...) = %v (%s), want %v (%s)",
					c.status, c.body, got, got.String(), c.want, c.want.String())
			}
		})
	}
}

// TestIsContextOverflow 验证判定函数：code 优先，截断/非 JSON 回落文案特征串。
func TestIsContextOverflow(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"code 命中", `{"code":11115,"msg":"input length too long"}`, true},
		{"code 截断（日志形态）", `{"code":11115,"msg":"input length too long","requestId":"abc","extError":{"code":"context_length_exceeded","type":"inva`, true},
		{"非 JSON 走文案", "input length too long", true},
		{"非 JSON 走上游码名", "context_length_exceeded: too big", true},
		{"大小写不敏感", "CONTEXT_LENGTH_EXCEEDED", true},

		{"1114 不命中", `{"code":1114}`, false},
		{"1114x 不命中", `{"code":11141}`, false},
		{"code 存在且非 11115 不做文案兜底", `{"code":11128,"msg":"context_length_exceeded"}`, false},
		{"空 body", ``, false},
		{"无关报文", `{"code":1,"msg":"bad request"}`, false},
		{"余量不足类文案不命中", `{"msg":"rate limit exceeded"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isContextOverflow(c.body); got != c.want {
				t.Errorf("isContextOverflow(%.60q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// TestErrContextOverflowString 类别名可读（日志与 GUI 探测页都要显示它）。
func TestErrContextOverflowString(t *testing.T) {
	s := ErrContextOverflow.String()
	if s == "" || strings.Contains(s, "none") {
		t.Fatalf("ErrContextOverflow.String()=%q 应有可读名称", s)
	}
}

// TestContextOverflowNotHardCredit 守住优先级：报文同时含"超限"与余额词时，
// 余额类别优先（它是账号级语义，处置是长冷却），不能被上下文超限截走。
func TestContextOverflowNotHardCredit(t *testing.T) {
	body := `{"code":11115,"msg":"input length too long","extError":{"code":"context_length_exceeded"},"hint":"余额不足"}`
	if got := Classify(400, body); got != ErrHardCredit {
		t.Errorf("Classify=%v, want ErrHardCredit（余额优先）", got)
	}
}

// TestContextOverflowDetail 验证日志抽取：有用的字段必须在，无关/敏感字段必须不在。
//
// 动机：正文截断到 200 字符时，extError 与 displayMsg 恰好被切掉——本次排查
// 正是因此只能靠反推 pi 的压缩阈值才定位到超限。此函数保证关键线索进日志。
func TestContextOverflowDetail(t *testing.T) {
	raw := `{"code":11115,"msg":"input length too long",` +
		`"requestId":"65150a4c-a3a4-4a6b-b28d-72b4b05eb9e5",` +
		`"extError":{"code":"context_length_exceeded","message":"input length too long","param":"","type":"invalid_request_error"},` +
		`"displayMsg":{"zh-hans":"对话内容超出模型长度上限，请精简对话或减少附件后重试。",` +
		`"zh-hant":"對話內容超出模型長度上限，請精簡對話或減少附件後重試。",` +
		`"en":"The conversation exceeds the model context limit."}}`
	got := contextOverflowDetail(raw)
	for _, want := range []string{
		"input length too long",
		"context_length_exceeded",
		"invalid_request_error",
		"对话内容超出模型长度上限",
		"The conversation exceeds the model context limit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail 缺少 %q\n got=%s", want, got)
		}
	}
	// 不打印 requestId（无诊断价值；且属可关联单次请求的标识）。
	if strings.Contains(got, "65150a4c") {
		t.Errorf("detail 不应输出 requestId: %s", got)
	}
	// 不输出繁中（与简中等价，徒增体积）。
	if strings.Contains(got, "對話內容") {
		t.Errorf("detail 不应输出 zh-hant（与 zh-hans 等价）: %s", got)
	}

	// 截断/非 JSON 形态：退回短截断，不 panic、不为空。
	got = contextOverflowDetail(`{"code":11115,"msg":"input length too long","extError":{"code":"context_`)
	if got == "" {
		t.Error("截断报文应退回短截断而非空串")
	}

	// 关键：报文里若混入长正文/凭证样式的字段，不得被原样倒进日志。
	leaky := `{"code":11115,"msg":"input length too long","messages":[{"role":"user","content":"` +
		strings.Repeat("SECRET-CONVERSATION ", 50) + `"}],"accessToken":"` + strings.Repeat("T", 200) + `"}`
	got = contextOverflowDetail(leaky)
	if strings.Contains(got, "SECRET-CONVERSATION") || strings.Contains(got, strings.Repeat("T", 40)) {
		t.Errorf("detail 泄露了对话正文/凭证: %s", got)
	}
}

// TestChatStreamContextOverflowLogLine 验证运行时日志真的带出体积与关键字段。
//
// 这条是本次排查的直接教训落地：旧日志只截断正文前 200 字符，恰好切掉
// extError/displayMsg，且完全不含请求体积，导致"到底多大、超了哪个限"无法
// 从日志读出。此测试用 httptest 假上游（不触真实上游、不消耗积分）断言：
//   - 记录 reqBytes 与 ~tok（近似 token，供判断超限幅度）；
//   - 带出 extError.code 与 displayMsg 文案；
//   - 不再走那条只有 200 字符正文的通用分支。
func TestChatStreamContextOverflowLogLine(t *testing.T) {
	const body = `{"code":11115,"msg":"input length too long","requestId":"abc",` +
		`"extError":{"code":"context_length_exceeded","type":"invalid_request_error"},` +
		`"displayMsg":{"zh-hans":"对话内容超出模型长度上限，请精简对话或减少附件后重试。"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL

	var buf bytes.Buffer
	oldW := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldW)

	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, respBody, _, err := c.ChatStreamWithParams(&auth.Auth{UID: "u1", AccessToken: "at"}, payload)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if rc != nil {
		t.Error("非 2xx 时 rc 应为 nil")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status=%d want 400", status)
	}
	if string(respBody) != body {
		t.Errorf("respBody 必须原样返回（供 handler 透传）:\n got=%.120s", respBody)
	}

	out := buf.String()
	for _, want := range []string{"context_overflow", "reqBytes=", "~tok=", "context_length_exceeded", "对话内容超出模型长度上限"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "abc") {
		t.Errorf("日志不应输出 requestId:\n%s", out)
	}
}
