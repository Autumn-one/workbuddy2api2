package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// ─────────────── 运行期开关代理（无需重启）───────────────
//
// 用户反馈：「代理功能怎么用？我怎么还得在 config.json 里设置参数、重启才行？
// 这也太难用了，搞简单一点行吗？」
//
// 现状：SetProxySelector 在启动时注入一次，闭包捕获当时的 registry——
// 运行期无法改（想把 nil 换成实际 selector 也不行）。
//
// 目标：GUI 上一个开关，点了立刻生效，不改配置、不重启。

// TestSetProxySelectorAtRuntime 运行期注入选择器应立即生效。
func TestSetProxySelectorAtRuntime(t *testing.T) {
	c := New()
	// 初始：无选择器 → 直连
	if c.ProxySelectorEnabled() {
		t.Fatal("初始应未启用（直连）")
	}
	called := false
	c.SetProxySelector(func(uid string) string {
		called = true
		return ""
	})
	if !c.ProxySelectorEnabled() {
		t.Fatal("运行期注入后 selector 应生效")
	}
	// 触发一次取值，确认新选择器被调用
	_ = c.clientFor(&auth.Auth{UID: "u1"}, false)
	if !called {
		t.Error("注入的选择器未被调用")
	}
}

// TestDisableProxyAtRuntime 运行期关闭代理（传回 nil）应立即回落直连。
func TestDisableProxyAtRuntime(t *testing.T) {
	c := New()
	c.SetProxySelector(func(string) string { return "http://127.0.0.1:1" })
	if !c.ProxySelectorEnabled() {
		t.Fatal("前置条件：selector 应已设置")
	}
	c.SetProxySelector(nil) // 关闭
	if c.ProxySelectorEnabled() {
		t.Error("关闭后应回落直连")
	}
}

// TestProxySelectorConcurrentSafe 运行期切换必须并发安全
// （请求可能正在其他 goroutine 里取值，切换不能引发数据竞争）。
func TestProxySelectorConcurrentSafe(t *testing.T) {
	c := New()
	done := make(chan struct{})
	// 并发读
	go func() {
		for i := 0; i < 2000; i++ {
			_ = c.clientFor(&auth.Auth{UID: "u1"}, false)
		}
		close(done)
	}()
	// 并发切换
	for i := 0; i < 500; i++ {
		if i%2 == 0 {
			c.SetProxySelector(func(string) string { return "http://127.0.0.1:1" })
		} else {
			c.SetProxySelector(nil)
		}
	}
	<-done
}
