package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeHTTPProxy 起一个最小 HTTP 正向代理（仅支持探测需要的 GET http://...），
// 把请求转发到 target。返回代理端口与关闭函数。
//
// 为什么不用 httptest.Server 的 Client 直接发：我们要测的是"经代理端口"这条链路，
// 必须真的有一个监听端口接受 CONNECT/绝对 URI 形式的代理请求。
func fakeHTTPProxy(t *testing.T, target string) (port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 绝对 URI 形式的代理请求：直接转发到目标（忽略 r.URL，探测目标固定）。
		resp, err := http.Get(target)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
	})}
	go srv.Serve(ln)
	return ln.Addr().(*net.TCPAddr).Port, func() { srv.Close(); ln.Close() }
}

// TestProbeDelaysMeasuresLatency 核心：经可用代理端口探测应得到 >0 的延迟。
func TestProbeDelaysMeasuresLatency(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	port, stop := fakeHTTPProxy(t, up.URL)
	defer stop()

	got := ProbeDelays([]Listener{{Name: "p", Node: "节点A", Port: port}}, 3*time.Second)
	if got["节点A"] <= 0 {
		t.Errorf("可用端口延迟=%d 应 >0", got["节点A"])
	}
}

// TestProbeDelaysLocal502IsUnhealthy 回归（实测缺陷）：Clash listener 在
// 代理名解析不到/组为空/出口失败时会本地秒回 502——请求没经过节点。
// 若把 5xx 记为成功，坏端口会显示"1ms 假健康"，账号永远绑在死端口上。
func TestProbeDelaysLocal502IsUnhealthy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer up.Close()
	port, stop := fakeHTTPProxy(t, up.URL)
	defer stop()

	got := ProbeDelays([]Listener{{Name: "p", Node: "坏节点", Port: port}}, 3*time.Second)
	if got["坏节点"] != 0 {
		t.Errorf("经代理拿到 502 应判不可用(0)，got %d", got["坏节点"])
	}
}

// TestProbeDelaysDeadPortIsZero 失败端口延迟必须为 0（不可用）。
func TestProbeDelaysDeadPortIsZero(t *testing.T) {
	got := ProbeDelays([]Listener{{Name: "p", Node: "死节点", Port: 1}}, 500*time.Millisecond)
	if got["死节点"] != 0 {
		t.Errorf("死端口延迟=%d 应 0", got["死节点"])
	}
}

// TestProbeDelaysConcurrentAllCovered 并发：每个节点都有结果，一个不漏
// （对比 Clash 批量接口会省略超时节点的缺陷）。
func TestProbeDelaysConcurrentAllCovered(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // 模拟慢节点
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()

	var ls []Listener
	var stops []func()
	for i := 0; i < 8; i++ {
		port, stop := fakeHTTPProxy(t, up.URL)
		stops = append(stops, stop)
		ls = append(ls, Listener{Name: fmt.Sprintf("n%d", i), Node: fmt.Sprintf("节点%d", i), Port: port})
	}
	defer func() {
		for _, s := range stops {
			s()
		}
	}()

	start := time.Now()
	got := ProbeDelays(ls, 3*time.Second)
	elapsed := time.Since(start)

	if len(got) != 8 {
		t.Errorf("结果数=%d want 8（一个节点都不能漏）", len(got))
	}
	for n, d := range got {
		if d <= 0 {
			t.Errorf("节点 %s 延迟=%d 应 >0", n, d)
		}
	}
	// 并发语义：8 个 150ms 的探测若串行需 >1.2s；并发应远小于它。
	if elapsed > 900*time.Millisecond {
		t.Errorf("并发探测耗时 %v，疑似被串行化", elapsed)
	}
}

// TestApplyDelaysAutoRebindsDeadNode 核心（用户要求）：绑在失败节点上的账号
// 应被自动换到【可达且负载最少】的节点——**不比较延迟**（只要可达，不计较快慢）。
func TestApplyDelaysAutoRebindsDeadNode(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "dead", Node: "死节点", Port: 40001, Region: RegionHK},
		{Name: "fast", Node: "快节点", Port: 40002, Region: RegionHK},
		{Name: "slow", Node: "慢节点", Port: 40003, Region: RegionJP},
	})
	r.EnsureAllAssigned([]string{"u-dead", "u-fast"})

	// 构造：u-dead 绑在死节点，u-fast 绑在快节点
	r.SetNodeForAccount("u-dead", "死节点")
	r.SetNodeForAccount("u-fast", "快节点")

	var events []RebindEvent
	r.SetAutoRebindHook(func(ev RebindEvent) { events = append(events, ev) })

	r.ApplyDelays(map[string]int{"死节点": 0, "快节点": 30, "慢节点": 200})

	// 死节点上的账号被换走；目标是【负载最少】的可达节点（慢节点空着），
	// 而不是 30ms 的快节点——延迟不参与选路（用户要求）。
	if got := r.NodeFor("u-dead"); got != "慢节点" {
		t.Errorf("u-dead 应被换到负载最少的 慢节点（不看延迟）, got %q", got)
	}
	// 健康节点上的账号不动（稳定优先）
	if got := r.NodeFor("u-fast"); got != "快节点" {
		t.Errorf("u-fast 不应被换, got %q", got)
	}
	if len(events) != 1 || events[0].UID != "u-dead" || events[0].ToNode != "慢节点" {
		t.Errorf("换绑事件不符: %+v", events)
	}
}

// TestApplyDelaysNoRebindWhenAllDead 全部失败时不换绑（防抖动把账号换来换去）。
func TestApplyDelaysNoRebindWhenAllDead(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Node: "节点A", Port: 40001, Region: RegionHK},
		{Name: "b", Node: "节点B", Port: 40002, Region: RegionJP},
	})
	r.EnsureAllAssigned([]string{"u1"})
	before := r.NodeFor("u1")

	r.ApplyDelays(map[string]int{"节点A": 0, "节点B": 0})

	if got := r.NodeFor("u1"); got != before {
		t.Errorf("全部失败时不应换绑: %q → %q", before, got)
	}
}

// TestApplyDelaysNoTCPResurrection 回归（原缺陷）：死节点不得因 TCP 端口通而复活。
// 现在健康判定只由 ApplyDelays 决定——本轮延迟失败就是不健康，直到下轮延迟成功。
func TestApplyDelaysNoTCPResurrection(t *testing.T) {
	r := NewRegistry([]Listener{{Name: "n", Node: "节点", Port: 40001, Region: RegionHK}})

	r.ApplyDelays(map[string]int{"节点": 0}) // 探测失败
	if r.Healthy(40001) {
		t.Fatal("延迟失败应判不健康")
	}
	// 模拟"TCP 端口通"（旧 HealthLoop 会 MarkHealthy 复活它）——
	// 现在唯一复活路径是下一轮 ApplyDelays 延迟成功：
	r.ApplyDelays(map[string]int{"节点": 42}) // 下一轮探测成功
	if !r.Healthy(40001) {
		t.Fatal("延迟恢复后应判健康")
	}
}
