package upstream

import (
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// ─────────────── 记录"本次请求实际用了哪个代理" ───────────────
//
// 用户要求：「你看看能不能确定是真的每个请求都走对应的代理，有没有日志可以证明这些」
//
// 现状：代理选择发生在 ChatStreamWithParams 内部（clientFor → proxyPool.transportFor），
// 调用方（handler）拿不到"这次用了哪个端口"，因此日志无法证明。
//
// 目标：让上游调用返回"实际使用的代理标识"，供请求日志展示——
// 使"每个请求走哪个出口"在日志里可查、可核对。

// TestChatStreamReportsEffectiveProxy 返回实际使用的代理标识。
func TestChatStreamReportsEffectiveProxy(t *testing.T) {
	proxySrv, hits := newProxyRecorder(t, "P")
	c := New()
	c.ChatBaseCN = "http://chat.example"
	c.SetProxySelector(func(uid string) string {
		if uid == "u1" {
			return proxySrv.URL
		}
		return ""
	})

	_, _, _, ep, err := c.ChatStreamWithParams(&auth.Auth{UID: "u1", AccessToken: "at"}, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if len(*hits) == 0 {
		t.Fatal("假代理未收到请求")
	}
	// ep.Proxy 必须是实际使用的代理（含 host:port，便于核对）
	if !strings.Contains(ep.Proxy, "127.0.0.1") {
		t.Errorf("应报告实际代理地址, got %q", ep.Proxy)
	}
}

// TestEffectiveProxyEmptyWhenDirect 直连时代理标识为空（日志显示 "-"）。
func TestEffectiveProxyEmptyWhenDirect(t *testing.T) {
	c := New()
	c.ChatBaseCN = "http://127.0.0.1:1" // 必然失败，但代理标识应已确定
	_, _, _, ep, _ := c.ChatStreamWithParams(&auth.Auth{UID: "u1"}, []byte(`{}`))
	if ep.Proxy != "" {
		t.Errorf("未配置代理时 ep.Proxy 应为空（表示直连）, got %q", ep.Proxy)
	}
}

// TestEffectiveProxyReportsUnreachableFallback 代理不可达回落直连时，
// 标识应反映"实际没用代理"（不能谎报走了代理）。
func TestEffectiveProxyReportsUnreachableFallback(t *testing.T) {
	c := New()
	c.ChatBaseCN = "http://127.0.0.1:1"
	c.SetProxySelector(func(string) string { return "http://127.0.0.1:1" }) // 不可达
	_, _, _, ep, _ := c.ChatStreamWithParams(&auth.Auth{UID: "u1"}, []byte(`{}`))
	if ep.Proxy != "" {
		t.Errorf("代理不可达回落直连时应报告直连（空）, got %q", ep.Proxy)
	}
}

// TestProxyLabelForLog 日志展示用的代理文案（host:port，去掉 scheme）。
func TestProxyLabelForLog(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://127.0.0.1:34567", "127.0.0.1:34567"},
		{"socks5://127.0.0.1:1080", "127.0.0.1:1080"},
		{"", "-"},
	}
	for _, c := range cases {
		if got := ProxyLabelForLog(c.in); got != c.want {
			t.Errorf("ProxyLabelForLog(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
