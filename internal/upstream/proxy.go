// proxy.go — 按账号选择出口代理（每个账号固定走一个本地 Clash 端口）。
//
// 背景：上游按 IP 处置账号，同 IP 多账号共用会被批量风控。给不同账号分配
// 不同本地代理端口（Clash listeners 各绑一个节点）= 不同出口 IP。
//
// 设计要点：
//   - 每个端口一个独立 *http.Transport（连接池隔离：不同出口不能复用连接）；
//   - 传输层复刻 New() 的调优参数（连接池、超时），保持行为一致；
//   - 【代理不可用时回落直连】：代理是增强手段，不能成为单点故障。
//     实测教训（2026-09-13）：Clash 尚未配置 listeners 时端口拒绝连接，
//     当时所有请求直接失败（连读模型列表都挂），这是不可接受的。
package upstream

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

// ProxySelector 按账号返回代理 URL；返回空串表示该账号不走代理（直连）。
// 由 GUI 装配层注入（实现来自 internal/proxy 的分配表）。
type ProxySelector func(uid string) string

// proxyCheckTimeout 代理端口可用性预检超时（本地端口，应极快）。
// 预检而不是"失败后重试"：重试会真实发两次请求（浪费+可能触发上游风控），
// 而本地 TCP 预检几乎无成本。
const proxyCheckTimeout = 600 * time.Millisecond

// proxyTolerance 端口连续失败多久后视为不可用（避免单次抖动就判死）。
const proxyTolerance = 30 * time.Second

// proxyPool 按代理地址缓存 *http.Transport（同端口复用连接池）+ 可用性状态。
type proxyPool struct {
	mu     sync.Mutex
	byAddr map[string]*http.Transport
	// failing 记录"最近一次预检失败"的时刻；用于：① 节流日志 ② 短期跳过该代理
	failing map[string]time.Time
	// logged 已提示过不可用的代理（每种代理只提示一次，避免刷屏）
	logged map[string]bool
	// headerTimeout 新建 Transport 时用（与 New() 的 ResponseHeaderTimeout 对齐）
	headerTimeout time.Duration
}

func newProxyPool(headerTimeout time.Duration) *proxyPool {
	return &proxyPool{
		byAddr:        map[string]*http.Transport{},
		failing:       map[string]time.Time{},
		logged:        map[string]bool{},
		headerTimeout: headerTimeout,
	}
}

// available 预检代理是否可用：TCP 能连上即认为可用。
//
// 局限（诚实说明）：只证明本地端口在监听，不保证出口节点真能通外网
// （Clash 可能接受连接但节点已挂）。真实链路诊断由业务请求的失败信号补充——
// 但那属于上游错误处理，不在这里额外发请求（避免成本与风控暴露）。
func (p *proxyPool) available(proxyURL string) bool {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		// 补默认端口（http 代理 80 / socks 1080 都不是本地 Clash 的常见形态，
		// 这里保守用 80，实际都会在配置里写明端口）
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, proxyCheckTimeout)
	if err != nil {
		p.noteFailure(proxyURL)
		return false
	}
	_ = conn.Close()
	p.noteSuccess(proxyURL)
	return true
}

// noteFailure 记录一次失败：节流日志 + 记录失败时刻。
func (p *proxyPool) noteFailure(proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 短期内已记录过 → 只更新时间戳，不重复打日志
	if _, seen := p.failing[proxyURL]; seen {
		p.failing[proxyURL] = now
		return
	}
	p.failing[proxyURL] = now
	if !p.logged[proxyURL] {
		p.logged[proxyURL] = true
		log.Printf("proxy_unreachable addr=%s 本地代理端口不可达，已回落直连（每类代理只提示一次；"+
			"若刚配置了 Clash listeners 请确认已重启 Clash）", redactProxy(proxyURL))
	}
}

// noteSuccess 代理恢复可用：清除失败标记并允许下次失败时再提示。
func (p *proxyPool) noteSuccess(proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.failing[proxyURL]; ok {
		delete(p.failing, proxyURL)
		delete(p.logged, proxyURL)
		log.Printf("proxy_recovered addr=%s 本地代理端口恢复可用", redactProxy(proxyURL))
	}
}

// redactProxy 日志用的代理标识（只保留主机端口，不含可能的凭据）。
func redactProxy(proxyURL string) string {
	if u, err := url.Parse(proxyURL); err == nil && u.Host != "" {
		return u.Host
	}
	return proxyURL
}

// transportFor 返回指定代理地址对应的 Transport；空地址/非法/不可达返回 nil。
func (p *proxyPool) transportFor(proxyURL string) *http.Transport {
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return nil
	}
	if !p.available(proxyURL) {
		return nil // 不可达 → 调用方回落直连
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tr, ok := p.byAddr[proxyURL]; ok {
		return tr
	}
	tr := &http.Transport{
		Proxy:               http.ProxyURL(u),
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 与 New() 保持一致：聊天 SSE 首字节前硬上限
		ResponseHeaderTimeout: p.headerTimeout,
	}
	p.byAddr[proxyURL] = tr
	return tr
}

// atomicProxySel 并发安全的代理选择器容器。
// 用 atomic.Pointer 包装函数值（函数值不能直接原子存取，用指针间接）。
type atomicProxySel struct {
	p atomic.Pointer[ProxySelector]
}

func (a *atomicProxySel) Load() ProxySelector {
	if fp := a.p.Load(); fp != nil {
		return *fp
	}
	return nil
}

func (a *atomicProxySel) Store(sel ProxySelector) {
	if sel == nil {
		a.p.Store(nil)
		return
	}
	a.p.Store(&sel)
}

// SetProxySelector 注入/更换账号级代理选择器；nil = 全部直连。
//
// 【运行期可调用】：GUI 的代理开关直接调它即可立刻生效，无需改配置或重启。
// 并发安全（请求可能在其它 goroutine 中取值）。
func (c *Client) SetProxySelector(sel ProxySelector) {
	c.proxySel.Store(sel)
}

// ProxySelectorEnabled 报告当前是否启用了代理（供界面显示开关状态）。
func (c *Client) ProxySelectorEnabled() bool {
	return c.proxySel.Load() != nil
}

// clientFor 返回该账号应使用的 *http.Client（含其专属 Transport）。
//
// chat 为 true 时返回"无总时长上限"的 client（聊天 SSE 用），
// 否则返回带 Timeout 的 client（短 RPC 用）——与既有 HTTP/ChatHTTP 的分工一致。
//
// 代理不可达时回落共享的直连 client（既有行为），保证代理故障不影响服务可用性。
func (c *Client) clientFor(a *auth.Auth, chat bool) *http.Client {
	base := c.HTTP
	if chat {
		base = c.chatHTTP()
	}
	sel := c.proxySel.Load()
	if sel == nil || a == nil {
		return base
	}
	if c.proxyPool == nil {
		return base
	}
	tr := c.proxyPool.transportFor(sel(a.UID))
	if tr == nil {
		return base // 未绑定或代理不可达 → 直连
	}
	if chat {
		return &http.Client{Timeout: 0, Transport: tr} // 与 ChatHTTP 同口径
	}
	return &http.Client{Timeout: base.Timeout, Transport: tr}
}
