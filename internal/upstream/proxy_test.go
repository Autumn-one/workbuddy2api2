package upstream

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// ─────────────── 按账号选择出口代理 ───────────────
//
// 验证核心不变量：
//  1. 配置了代理的账号，请求真的经过该代理（不是"看起来配了但没生效"）；
//  2. 每个账号走自己的代理（互不串）；
//  3. 未配置 / 代理非法 → 回落直连，不报错（不破坏既有行为）。

// newProxyRecorder 起一个记录"被谁访问"的假代理。
// 它同时充当代理和目标：Go 的 HTTP 代理在收到绝对 URL 请求时会用它转发，
// 这里直接应答，从而证明"请求确实经过了本代理"。
func newProxyRecorder(t *testing.T, tag string) (*httptest.Server, *[]string) {
	t.Helper()
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, tag+" "+r.URL.String())
		// 返回 200 + 合法 SSE，让调用链正常结束（用 403 会被 Go 当传输层错误）
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"usage\":{\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestProxySelectorUsed 配置的代理必须真正生效。
func TestProxySelectorUsed(t *testing.T) {
	proxySrv, hits := newProxyRecorder(t, "P1")
	c := New()
	c.ChatBaseCN = "http://chat.example"
	c.SetProxySelector(func(uid string) string { return proxySrv.URL })

	_, status, _, err := c.ChatStream(&auth.Auth{UID: "u1", AccessToken: "at"}, []byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200（应经过假代理）", status)
	}
	if len(*hits) == 0 {
		t.Fatal("假代理未收到任何请求 → 代理未生效")
	}
	if !strings.Contains((*hits)[0], "chat.example") {
		t.Errorf("代理收到的目标地址不对: %v", (*hits)[0])
	}
}

// TestPerAccountProxyIsolated 每个账号走各自的代理，互不串。
func TestPerAccountProxyIsolated(t *testing.T) {
	p1, hits1 := newProxyRecorder(t, "P1")
	p2, hits2 := newProxyRecorder(t, "P2")

	c := New()
	c.ChatBaseCN = "http://chat.example"
	c.SetProxySelector(func(uid string) string {
		switch uid {
		case "u1":
			return p1.URL
		case "u2":
			return p2.URL
		}
		return "" // 其他账号直连
	})

	for _, uid := range []string{"u1", "u2", "u1"} {
		_, _, _, err := c.ChatStream(&auth.Auth{UID: uid, AccessToken: "at"}, []byte(`{}`))
		if err != nil {
			t.Fatalf("%s 请求失败: %v", uid, err)
		}
	}
	if len(*hits1) != 2 {
		t.Errorf("u1 应经 P1 两次, got %d", len(*hits1))
	}
	if len(*hits2) != 1 {
		t.Errorf("u2 应经 P2 一次, got %d", len(*hits2))
	}
}

// TestNoProxySelectorFallsBack 未注入选择器 → 直连（既有行为不变）。
func TestNoProxySelectorFallsBack(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer target.Close()

	c := New()
	c.ChatBaseCN = target.URL // 直接指向目标，不经过代理
	_, status, _, err := c.ChatStream(&auth.Auth{UID: "u1", AccessToken: "at"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("直连请求失败: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status=%d want 200", status)
	}
}

// TestInvalidProxyURLFallsBack 代理 URL 非法 → 回落直连，不得让请求失败。
func TestInvalidProxyURLFallsBack(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer target.Close()

	c := New()
	c.ChatBaseCN = target.URL
	c.SetProxySelector(func(string) string { return "://bad-url" })
	_, status, _, err := c.ChatStream(&auth.Auth{UID: "u1", AccessToken: "at"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("非法代理应回落直连而非报错: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status=%d want 200（回落直连成功）", status)
	}
}

// TestProxyTransportCached 同一代理地址复用同一 Transport（连接池不重复建）。
// 需要真实监听的端口：transportFor 会先做可用性预检（端口不可达则回落，返回 nil）。
func TestProxyTransportCached(t *testing.T) {
	p1 := listenLocal(t)
	p2 := listenLocal(t)
	c := New()
	tr1 := c.proxyPool.transportFor("http://" + p1)
	tr2 := c.proxyPool.transportFor("http://" + p1)
	if tr1 == nil || tr1 != tr2 {
		t.Error("同一代理地址应复用同一 Transport")
	}
	tr3 := c.proxyPool.transportFor("http://" + p2)
	if tr3 == nil || tr3 == tr1 {
		t.Error("不同代理地址应是不同 Transport（出口隔离）")
	}
}

// listenLocal 起一个本地 TCP 监听并返回其 host:port（供代理预检通过）。
func listenLocal(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// TestProxyTransportHasProxySet 生成的 Transport 必须真的设置了 Proxy。
func TestProxyTransportHasProxySet(t *testing.T) {
	addr := listenLocal(t)
	c := New()
	tr := c.proxyPool.transportFor("http://" + addr)
	if tr == nil || tr.Proxy == nil {
		t.Fatal("Transport.Proxy 未设置 → 请求不会走代理")
	}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil {
		t.Fatalf("Proxy(req) 失败: %v %v", u, err)
	}
	if u.Host != addr {
		t.Errorf("代理地址=%v want %v", u, addr)
	}
}

// TestShortRPCUsesProxy 短 RPC（签到/余额/refresh）也必须走代理——
// 否则"聊天走代理、签到走直连"本身就是矛盾特征。
func TestShortRPCUsesProxy(t *testing.T) {
	proxySrv, hits := newProxyRecorder(t, "P")
	c := New()
	c.ChatBaseCN = "http://chat.example"
	c.BillingBaseCN = "http://billing.example"
	c.SetProxySelector(func(string) string { return proxySrv.URL })

	// UserResource 走 billing 短 RPC
	_, _ = c.UserResource(&auth.Auth{UID: "u1", AccessToken: "at"})

	if len(*hits) == 0 {
		t.Fatal("短 RPC 未经过代理")
	}
	if !strings.Contains((*hits)[0], "billing.example") {
		t.Errorf("短 RPC 目标地址不对: %v", (*hits)[0])
	}
}

// TestProxyURLParsedCorrectly 端口 → 代理 URL 的解析（含 socks5 兼容）。
func TestProxyURLParsedCorrectly(t *testing.T) {
	c := New()
	live := listenLocal(t)
	for _, pfx := range []string{"http://", "socks5://"} {
		addr := pfx + live
		tr := c.proxyPool.transportFor(addr)
		if tr == nil {
			t.Errorf("%s 应生成 Transport", addr)
			continue
		}
		req, _ := http.NewRequest("GET", "https://x.com", nil)
		u, err := tr.Proxy(req)
		if err != nil || u == nil {
			t.Errorf("%s: Proxy(req) 失败", addr)
			continue
		}
		want, _ := url.Parse(addr)
		if u.Host != want.Host {
			t.Errorf("%s → %s 不匹配", addr, u.Host)
		}
	}
}

// ─────────────── 代理不可用时的回落 ───────────────
//
// 生产实测（2026-09-13）：Clash 尚未配置 listeners 时，端口 34567 拒绝连接，
// 网关的【所有请求直接失败】——包括读模型列表、签到、余额查询。
//
// 这是不可接受的：代理是"降低风控风险"的增强手段，不应成为单点故障。
// 代理连不上时必须回落直连（既有行为），而不是让整个网关瘫痪。

// TestProxyUnreachableFallsBackToDirect 代理端口拒绝连接 → 回落直连成功。
func TestProxyUnreachableFallsBackToDirect(t *testing.T) {
	// 目标直连可用
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer target.Close()

	c := New()
	c.ChatBaseCN = target.URL
	// 指向一个几乎必然拒绝连接的端口
	c.SetProxySelector(func(string) string { return "http://127.0.0.1:1" })

	_, status, _, err := c.ChatStream(&auth.Auth{UID: "u1", AccessToken: "at"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("代理不可用时应回落直连，而不是报错: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status=%d want 200（回落直连）", status)
	}
}

// TestProxyUnreachableShortRPCFallsBack 短 RPC 同样必须回落。
func TestProxyUnreachableShortRPCFallsBack(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer target.Close()

	c := New()
	c.BillingBaseCN = target.URL
	c.SetProxySelector(func(string) string { return "http://127.0.0.1:1" })

	if _, err := c.UserResource(&auth.Auth{UID: "u1", AccessToken: "at"}); err != nil {
		t.Fatalf("短 RPC 在代理不可用时应回落直连: %v", err)
	}
}

// TestProxyFailureLogsOnce 回落时应有可观测日志（不能静默），但不能每请求刷屏。
func TestProxyFailureLogsOnce(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer target.Close()

	c := New()
	c.ChatBaseCN = target.URL
	c.SetProxySelector(func(string) string { return "http://127.0.0.1:1" })

	var buf bytes.Buffer
	oldW := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldW)

	for i := 0; i < 3; i++ {
		_, _, _, _ = c.ChatStream(&auth.Auth{UID: "u1", AccessToken: "at"}, []byte(`{}`))
	}
	out := buf.String()
	if !strings.Contains(out, "proxy") {
		t.Errorf("代理回落应有日志提示:\n%s", out)
	}
	// 不应每请求刷屏（同一账号同一代理只提示一次）
	if n := strings.Count(out, "proxy_unreachable"); n > 1 {
		t.Errorf("代理不可用日志重复 %d 次（应节流）:\n%s", n, out)
	}
}
