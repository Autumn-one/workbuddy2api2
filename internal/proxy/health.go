// health.go — 代理端口健康探测（低成本）。
//
// 用户要求："定期以低成本的方式去检测这个代理节点是否通"。
//
// 成本控制（关键）：
//   - 只做【TCP 连通性】探测本地 Clash 端口——本地连接，不消耗真实流量、
//     不打上游、不计费。端口能握上手说明 Clash 在监听该 listener。
//   - 不做真实业务请求（那才会消耗流量/积分，且会暴露给上游）。
//
// 局限（必须诚实说明）：TCP 连通只证明"本地端口活着"，不保证出口节点真能通外网
// （Clash 可能接受连接但节点本身挂了）。真实链路验证需要发一次外部请求——
// 由调用方在"确实要发业务请求"时顺带判定：业务请求失败且是网络层错误 → 标记不健康。
// 这样既不额外花钱，又能拿到真实信号。
package proxy

import (
	"context"
	"net"
	"sync"
	"time"
)

// ProbeTimeout 单次 TCP 探测超时（本地端口，通常 <10ms；给足余量）。
const ProbeTimeout = 2 * time.Second

// ProbePort 探测单个本地代理端口是否可连接。返回 true 表示可连接。
func ProbePort(port int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addrOf(port))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// addrOf 本地回环地址（Clash 默认只监听 127.0.0.1）。
func addrOf(port int) string {
	return "127.0.0.1:" + itoa(port)
}

// itoa 避免引 strconv（本文件只此一处需要）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ProbeAll 并发探测所有 listener，返回 端口 → 是否可连接。
// 并发是为了不让串行探测拖长周期（本地连接很快，并发无副作用）。
func ProbeAll(ls []Listener) map[int]bool {
	out := make(map[int]bool, len(ls))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, l := range ls {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			ok := ProbePort(port)
			mu.Lock()
			out[port] = ok
			mu.Unlock()
		}(l.Port)
	}
	wg.Wait()
	return out
}

// HealthLoop 定期探测并更新注册表健康状态，直到 ctx 结束。
//
// 只标记"探测失败"为不健康；探测成功会清除不健康标记（节点恢复后自动回归）。
// 绑定关系不受影响：健康状态只影响【新分配】与【Rebind 的可选目标】，
// 已绑定账号仍走原节点（稳定性优先，除非调用方显式 Rebind）。
func (r *Registry) HealthLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHealthInterval
	}
	// 启动时先探一次，避免开局就用到坏节点。
	r.probeOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.probeOnce()
		}
	}
}

// probeOnce 探测一轮并更新健康标记。
func (r *Registry) probeOnce() {
	ls := r.Listeners()
	if len(ls) == 0 {
		return
	}
	results := ProbeAll(ls)
	for port, ok := range results {
		if ok {
			r.MarkHealthy(port)
		} else {
			r.MarkUnhealthy(port)
		}
	}
}
