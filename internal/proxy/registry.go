// Package proxy 账号级代理分配：把每个账号绑定到一个固定的本地代理端口。
//
// 背景（用户实测）：上游开始按 IP 处置账号，同一 IP 下多账号共用会被批量风控
// （换 IP 后即可正常注册，证实是 IP 维度）。因此需要给账号分散出口 IP。
//
// 实现方式（关键设计）：本地 Clash/mihomo 用 listeners 给每个节点开一个专属端口
// （如 34567 → 香港Y01），网关按"账号 → 端口"映射发请求。
//
// 为什么不用 Clash API 动态切节点：Clash 的节点选择是【全局状态】，切换会影响
// 所有并发连接——网关是并发的，切一次就会互相踩。固定端口方案每个连接独立选出口，
// 与并发天然兼容。
//
// 约束（均来自用户明确要求）：
//  1. 同一账号稳定走同一节点（IP 频繁变化本身就是异常特征）；
//  2. 节点不通时换到健康的节点（定期低成本探测）；
//  3. 节点不足时允许一个节点服务多个账号；
//  4. 优先 HK / TW / JP，其他地区排后。
package proxy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultHealthInterval 健康检查周期（低成本：只做 TCP 连通性探测，不发业务请求）。
const DefaultHealthInterval = 5 * time.Minute

// Region 节点地区（决定分配优先级）。
type Region int

const (
	RegionHK    Region = iota // 香港（最高优先）
	RegionTW                  // 台湾
	RegionJP                  // 日本
	RegionOther               // 其他地区（排后）
)

// String 可读地区名（界面/日志用）。
func (r Region) String() string {
	switch r {
	case RegionHK:
		return "香港"
	case RegionTW:
		return "台湾"
	case RegionJP:
		return "日本"
	default:
		return "其他"
	}
}

// RegionFromNodeName 从节点名识别地区。
// 按用户指定的优先级：HK → TW → JP → 其他。
func RegionFromNodeName(node string) Region {
	u := strings.ToUpper(node)
	switch {
	case strings.Contains(node, "香港") || strings.Contains(u, "HK"):
		return RegionHK
	case strings.Contains(node, "台湾") || strings.Contains(u, "TW"):
		return RegionTW
	case strings.Contains(node, "日本") || strings.Contains(u, "JP"):
		return RegionJP
	default:
		return RegionOther
	}
}

// Listener 一个本地代理端口（对应 Clash 里的一条 listeners 配置）。
type Listener struct {
	Name   string // listener 名（acct-01-HK）
	Port   int
	Node   string // 上游节点名（原样保留，界面显示这个可读名）
	Region Region
}

// ProxyURL 该 listener 的本地代理地址（HTTP 代理；Clash 的 mixed 端口同时支持 HTTP/SOCKS）。
func (l Listener) ProxyURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", l.Port)
}

// Binding 账号 → 节点的绑定（界面展示用）。
type Binding struct {
	UID    string
	Node   string // 可读节点名（不是 URL）
	Port   int
	Region Region
}

// Registry 账号到代理端口的分配表（线程安全）。
type Registry struct {
	mu        sync.RWMutex
	listeners []Listener        // 按地区优先级排好序
	byUID     map[string]int    // uid → listeners 下标
	unhealthy map[int]time.Time // 端口 → 标记不健康的时间
}

// NewRegistry 构建注册表；listeners 会按地区优先级稳定排序。
func NewRegistry(ls []Listener) *Registry {
	sorted := make([]Listener, len(ls))
	copy(sorted, ls)
	// 稳定排序：先地区（HK→TW→JP→其他），再端口（保证可复现，不受输入顺序影响）
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Region != sorted[j].Region {
			return sorted[i].Region < sorted[j].Region
		}
		return sorted[i].Port < sorted[j].Port
	})
	return &Registry{
		listeners: sorted,
		byUID:     map[string]int{},
		unhealthy: map[int]time.Time{},
	}
}

// Listeners 返回排序后的 listener 列表副本。
func (r *Registry) Listeners() []Listener {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Listener, len(r.listeners))
	copy(out, r.listeners)
	return out
}

// Healthy 报告某端口当前是否可用（未被标记不健康）。
func (r *Registry) Healthy(port int) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, bad := r.unhealthy[port]
	return !bad
}

// MarkHealthy 标记端口可用（探测成功）。
func (r *Registry) MarkHealthy(port int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.unhealthy, port)
}

// MarkUnhealthy 标记端口不可用（探测失败）。
func (r *Registry) MarkUnhealthy(port int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.unhealthy[port]; !ok {
		r.unhealthy[port] = time.Now()
	}
}

// Assign 返回账号应使用的 listener（稳定绑定）。
//
// 分配策略：
//  1. 已绑定 → 直接返回（稳定性优先；即使该节点当前不健康也返回，由调用方决定是否 Rebind）；
//  2. 未绑定 → 优先分配给【健康且负载最少】的 listener；同负载时优先地区靠前的；
//  3. 全部不健康 → 仍返回一个（回落：好过完全没有代理）；
//  4. 无 listener → ok=false，调用方回落直连。
func (r *Registry) Assign(uid string) (Listener, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assignLocked(uid)
}

func (r *Registry) assignLocked(uid string) (Listener, bool) {
	if len(r.listeners) == 0 {
		return Listener{}, false
	}
	if idx, ok := r.byUID[uid]; ok && idx < len(r.listeners) {
		return r.listeners[idx], true
	}
	// 选负载最少的健康节点；同负载取地区靠前（listeners 已按优先级排序）。
	load := r.loadLocked()
	best, bestLoad := -1, 1<<30
	for i, l := range r.listeners {
		if _, bad := r.unhealthy[l.Port]; bad {
			continue
		}
		if load[l.Port] < bestLoad {
			best, bestLoad = i, load[l.Port]
		}
	}
	if best < 0 {
		// 全部不健康：回落第一个（宁可走一个可能不通的出口，也不放弃代理）
		best = 0
	}
	r.byUID[uid] = best
	return r.listeners[best], true
}

// loadLocked 统计每个端口当前服务的账号数。调用方需已持锁。
func (r *Registry) loadLocked() map[int]int {
	load := make(map[int]int, len(r.listeners))
	for _, idx := range r.byUID {
		if idx >= 0 && idx < len(r.listeners) {
			load[r.listeners[idx].Port]++
		}
	}
	return load
}

// Rebind 强制账号换一个节点（当前节点不通时用）。返回新的绑定。
func (r *Registry) Rebind(uid string) (Listener, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.listeners) == 0 {
		return Listener{}, false
	}
	cur, hadCur := r.byUID[uid]
	load := r.loadLocked()
	curPort := -1
	if hadCur && cur < len(r.listeners) {
		curPort = r.listeners[cur].Port
		load[curPort]-- // 排除自己，避免"自己占着位置"影响选择
	}
	best, bestLoad := -1, 1<<30
	for i, l := range r.listeners {
		if l.Port == curPort {
			continue // 换就是要换掉当前这个
		}
		if _, bad := r.unhealthy[l.Port]; bad {
			continue
		}
		if load[l.Port] < bestLoad {
			best, bestLoad = i, load[l.Port]
		}
	}
	if best < 0 {
		return Listener{}, false // 没有别的健康节点可换
	}
	r.byUID[uid] = best
	return r.listeners[best], true
}

// Unassign 解除账号绑定（账号删除时调用，释放节点占用）。
func (r *Registry) Unassign(uid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byUID, uid)
}

// EnsureAllAssigned 一次性为给定账号建立绑定（已绑定的保持不变）。
//
// 用途（修设计缺陷）：界面开启代理后需要【立刻】看到全部账号各自绑到哪个节点。
// 此前绑定是懒执行的（要等请求打到该账号才 Assign），导致列表默认空白——
// 用户以为功能没生效。现在开启时调用本方法一次填满，且不改动已有绑定（IP 稳定）。
func (r *Registry) EnsureAllAssigned(uids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if _, ok := r.byUID[uid]; ok {
			continue // 已有绑定：保持不动（IP 必须稳定）
		}
		r.assignLocked(uid)
	}
}

// Snapshot 返回当前所有绑定（界面展示"哪个账号在用哪个节点"）。
func (r *Registry) Snapshot() []Binding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Binding, 0, len(r.byUID))
	for uid, idx := range r.byUID {
		if idx < 0 || idx >= len(r.listeners) {
			continue
		}
		l := r.listeners[idx]
		out = append(out, Binding{UID: uid, Node: l.Node, Port: l.Port, Region: l.Region})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// NodeFor 返回账号当前绑定的节点名（未绑定返回空串）。
func (r *Registry) NodeFor(uid string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	idx, ok := r.byUID[uid]
	if !ok || idx >= len(r.listeners) {
		return ""
	}
	return r.listeners[idx].Node
}
