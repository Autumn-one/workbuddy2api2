// health.go — 代理节点健康探测（真实延迟驱动）。
//
// 探测方式：经每个节点的【本地端口】发 generate_204（见 delayprobe.go）。
// 这是与业务流量完全同一条链路，测出的延迟就是真实延迟，且：
//   - 不会漏节点（批量 Clash API 会省略超时节点，实测冤枉过活节点）；
//   - 判决权唯一——健康只由延迟决定，不再被 TCP 端口探测覆盖
//     （原缺陷：死节点被 TCP 探测 MarkHealthy 复活，账号一直绑在死节点上）。
//
// 成本：每节点每周期一次 generate_204（几字节），不经本网关、不消耗上游模型积分。
// 不依赖外网可达性之外的条件：无外网时全部延迟失败 → 全不健康 → 网关回落直连，
// 与"节点全挂"的行为一致（宁可直连也不卡死）。
package proxy

import (
	"context"
	"net"
	"sync"
	"time"
)

// ProbeTimeout 单次 TCP 探测超时（本地端口，通常 <10ms；给足余量）。
const ProbeTimeout = 2 * time.Second

// DelayProbeTimeout 单节点延迟探测超时（经本地端口发 generate_204 的总时限）。
// 3s 与 Clash 延迟接口的超时口径一致：超过即认为"该节点现在不可用"。
const DelayProbeTimeout = 3 * time.Second

// ProbePort 探测单个本地代理端口是否可连接。返回 true 表示可连接。
//
// 保留用途：诊断分层——区分"Clash 没在监听端口"（配置/Clash 问题）与
// "端口在监听但节点不通"（节点问题）。不再参与健康判定（见文件头注释）。
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
//
// 保留用途：诊断提示（例如"全部延迟失败时，是 Clash 挂了还是节点全挂了"）。
// 不参与健康判定——健康只由 ProbeDelays 的延迟结果决定。
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

// HealthLoop 定期探测并更新注册表健康状态与延迟缓存，直到 ctx 结束。
//
// 每轮 ProbeDelays + ApplyDelays：失败节点上的账号被自动换绑（ApplyDelays 内部）。
// 已绑定的健康节点不受影响（稳定优先）；恢复健康的节点也不会把账号"抢回来"——
// 出口 IP 稳定优先于"回到老节点"。
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

// probeOnce 探测一轮：经每个节点本地端口发 generate_204，把结果写进注册表。
// 延迟 > 0 → 健康；失败/超时 → 不健康，并触发 ApplyDelays 内部的自动换绑。
func (r *Registry) probeOnce() {
	ls := r.Listeners()
	if len(ls) == 0 {
		return
	}
	delays := ProbeDelays(ls, DelayProbeTimeout)
	r.ApplyDelays(delays)
}
