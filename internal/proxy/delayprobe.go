// delayprobe.go — 经【节点本地端口】的真实延迟探测。
//
// 为什么替代 Clash 批量延迟接口（/group/<组>/delay）：
//  1. 批量接口会漏节点——超时窗口内没出结果的节点被直接省略，活节点也被冤枉
//     （实测：香港Y10 单独探测 36ms，批量结果里缺席）。
//  2. 批量接口的延迟是"Clash 控制面代表探测"，与业务流量路径不同；
//     经本地端口探测 = 与业务请求完全同一条链路（本地端口 → 节点 → 目标），
//     测出的延迟就是用户真实会经历的延迟。
//  3. 判决权唯一：健康只由本探测决定，不再被 TCP 端口探测覆盖
//     （原缺陷：死节点被 TCP 探测 MarkHealthy 复活，账号一直绑在死节点上）。
//
// 成本：每个节点一次 generate_204（几字节），不经本网关、不消耗上游模型积分；
// 并发发起，总耗时 ≈ 最慢节点的耗时（≤ timeout + 少许调度开销）。
package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ProbeDelays 并发探测每个 listener 的真实延迟，返回 节点名 → 毫秒（0 = 失败/超时）。
//
// 一个端口一个 goroutine：46 个节点并发后总耗时 ≈ 最慢节点，远快于串行。
// 失败语义与 Clash 延迟接口对齐：0 = 不可用（区分不出端口不通还是节点死，
// 但对"要不要用这个节点"而言两者等价——都不能用）。
func ProbeDelays(ls []Listener, timeout time.Duration) map[string]int {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	out := make(map[string]int, len(ls))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Add(1)
		go func(l Listener) {
			defer wg.Done()
			d := probeOneDelay(l.Port, timeout)
			mu.Lock()
			out[l.Node] = d
			mu.Unlock()
		}(l)
	}
	wg.Wait()
	return out
}

// probeOneDelay 经单个本地端口发 generate_204，返回延迟毫秒；任何失败返回 0。
func probeOneDelay(port int, timeout time.Duration) int {
	u, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return 0
	}
	tr := &http.Transport{Proxy: http.ProxyURL(u)}
	defer tr.CloseIdleConnections()
	cli := &http.Client{Transport: tr, Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DelayTestURL, nil)
	if err != nil {
		return 0
	}
	start := time.Now()
	resp, err := cli.Do(req)
	if err != nil {
		return 0
	}
	// 读空 body 再关闭：让连接可复用（虽然本探测一个连接只用一次，
	// 但读完能避免底层把"半截响应"记为连接错误）。
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// 任意 HTTP 状态码都说明"请求经节点到达目标并拿到了响应"——链路是通的。
	// （generate_204 正常返回 204；即使被劫持成别的码，通就是通。）
	d := time.Since(start).Milliseconds()
	if d <= 0 {
		return 1 // 本地回环快得测不出毫秒级，记 1 以区别于失败 0
	}
	return int(d)
}
