package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestProbePortRealListener 真实起一个本地监听，验证探测能识别"通"与"不通"。
func TestProbePortRealListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if !ProbePort(port) {
		t.Errorf("正在监听的端口 %d 应探测为可连接", port)
	}
	// 关闭后再探（挑一个确定没监听的端口：用刚释放的端口）
	ln.Close()
	time.Sleep(50 * time.Millisecond)
	if ProbePort(port) {
		t.Errorf("已关闭的端口 %d 应探测为不可连接", port)
	}
}

// TestProbeAllMixed 混合场景：通与不通都要如实反映。
func TestProbeAllMixed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	live := ln.Addr().(*net.TCPAddr).Port
	dead := 1 // 1 号端口几乎必然不可连接

	got := ProbeAll([]Listener{
		{Name: "live", Port: live},
		{Name: "dead", Port: dead},
	})
	if !got[live] {
		t.Errorf("live 端口应 ok: %v", got)
	}
	if got[dead] {
		t.Errorf("dead 端口应 not ok: %v", got)
	}
}

// TestHealthLoopUpdatesRegistry 健康循环应把探测结果写进注册表。
// TestHealthLoopUpdatesRegistry 健康循环应把【延迟探测】结果写进注册表：
// 能转发 generate_204 的端口判健康（且延迟入缓存），不能转发的判不健康。
// live 用真 HTTP 代理（转发到 httptest 目标），dead 用必然不可连接的 1 号端口。
func TestHealthLoopUpdatesRegistry(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	live, stop := fakeHTTPProxy(t, up.URL)
	defer stop()

	r := NewRegistry([]Listener{
		{Name: "live", Port: live, Node: "香港", Region: RegionHK},
		{Name: "dead", Port: 1, Node: "日本", Region: RegionJP},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.HealthLoop(ctx, 50*time.Millisecond)

	// 等第一轮探测完成（HealthLoop 启动时立即探一次）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.Healthy(live) && !r.Healthy(1) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !r.Healthy(live) {
		t.Error("live 端口应被标记健康")
	}
	if r.Healthy(1) {
		t.Error("dead 端口应被标记不健康")
	}
	if d := r.DelayOf("香港"); d <= 0 {
		t.Errorf("live 节点延迟应入缓存（>0）, got %d", d)
	}
}

// TestProbeLoopNoListeners 无 listener 时循环不 panic（空配置安全）。
func TestProbeLoopNoListeners(t *testing.T) {
	r := NewRegistry(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { r.HealthLoop(ctx, 20*time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("空配置的 HealthLoop 应及时退出")
	}
}

// TestProbeTimeoutReasonable 探测超时必须够短（本地端口），避免拖长周期。
func TestProbeTimeoutReasonable(t *testing.T) {
	if ProbeTimeout <= 0 || ProbeTimeout > 5*time.Second {
		t.Fatalf("ProbeTimeout=%v 不合理（本地探测应短）", ProbeTimeout)
	}
}
