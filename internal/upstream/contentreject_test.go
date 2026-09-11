package upstream

import (
	"strings"
	"testing"
)

// ─────────────── 11140 内容安全拒绝（账号级风控）───────────────
//
// 生产实证（2026-09-12，gui.log）：
//   账号 eedf4e88：11140 共 189 次，成功 0 次（100% 被拒）
//   账号 d4937369：11140 共 4 次，成功 0 次
//   其他 10 个账号：11140 共 0 次，成功 1000+ 次
//   且每次都是「账号 X 撞 11140 → 换号 → 立刻 200」——同一个请求体，
//   换账号就通过，证明【不是内容问题，而是账号被上游风控标记】。
//
// 此前 11140 被归入 ErrClient（"客户端错误，只换号不罚"），导致被标记的账号
// 每次被选中都白撞一次，流量跟着轮换空跑。需要独立分类 + 短期冷却。

// real11140Body 生产日志里的真实报文（截取到可解析的部分）。
const real11140Body = `{"code":11140,"msg":"request illegal","requestId":"7c140f42-13ce-4a31-82d1-8d929b23fdbf","displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.","zh":"内容未通过安全审核"}}`

// TestClassify11140 11140 必须归类为专用类别，而不是混在 ErrClient 里。
func TestClassify11140(t *testing.T) {
	got := Classify(403, real11140Body)
	if got != ErrContentRejected {
		t.Fatalf("Classify(403, 11140)=%v want ErrContentRejected", got)
	}
}

// TestClassify11140VariousStatus 上游可能换状态码返回：应按 code 而非状态码识别。
func TestClassify11140VariousStatus(t *testing.T) {
	for _, st := range []int{400, 403} {
		if got := Classify(st, real11140Body); got != ErrContentRejected {
			t.Errorf("Classify(%d, 11140)=%v want ErrContentRejected", st, got)
		}
	}
}

// TestClassifyNot11140 不含 11140 的普通 403 仍是 ErrClient（不得误吸）。
func TestClassifyNot11140(t *testing.T) {
	body := `{"code":8888,"msg":"some other client error"}`
	if got := Classify(403, body); got != ErrClient {
		t.Errorf("普通 403 应仍为 ErrClient, got %v", got)
	}
}

// TestClassify11140DoesNotShadowOthers 11140 判定不得抢走其它类别的分类：
// 余额不足 / session 失效 / 6004 优先级都更高。
func TestClassify11140DoesNotShadowOthers(t *testing.T) {
	// 余额不足优先
	body := `{"code":11140,"msg":"余额不足"}`
	if got := Classify(403, body); got != ErrHardCredit {
		t.Errorf("含余额关键词应优先判为 ErrHardCredit, got %v", got)
	}
	// session 失效优先（同报文里出现 12153）
	body2 := `{"code":12153,"msg":"Offline user session not found"}`
	if got := Classify(401, body2); got != ErrSessionDead {
		t.Errorf("session 失效应为 ErrSessionDead, got %v", got)
	}
	// 6004 优先
	body3 := `{"code":6004,"msg":"您对模型的使用量已超出频率限制，可切换其他模型"}`
	if got := Classify(429, body3); got != ErrModelRateLimit {
		t.Errorf("6004 应为 ErrModelRateLimit, got %v", got)
	}
}

// TestIsContentRejection 识别函数对缺字段/畸形 JSON 必须安全（不得 panic）。
func TestIsContentRejection(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{real11140Body, true},
		{`{"code":11140}`, true},
		{`{"code": 11140 ,"msg":"x"}`, true},
		{`{"code":"11140"}`, false}, // 字符串型 code：保守不认（避免误判）
		{`{"code":11141}`, false},
		{`{"code":1114}`, false}, // 子串不得误命中（关键：1114 是 11140 的前缀）
		{`{}`, false},
		{`not json`, false},
		{``, false},
		{`{"code":11140`, false}, // 截断 JSON
	}
	for _, c := range cases {
		if got := isContentRejection(c.body); got != c.want {
			t.Errorf("isContentRejection(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestErrContentRejectedString 类别名可读（日志/文档要显示它）。
func TestErrContentRejectedString(t *testing.T) {
	s := ErrContentRejected.String()
	if s == "" || strings.Contains(s, "unknown") {
		t.Fatalf("ErrContentRejected.String()=%q 应有可读名称", s)
	}
}
