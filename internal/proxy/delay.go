// delay.go — 真实连通检测（节点延迟）与节点选择。
//
// 背景（用户提问）：原健康检测只做 TCP 连通，只能证明"Clash 在监听端口"，
// 不能证明出口节点真能通外网（Clash 可能接受连接但节点已挂）。
//
// 实测发现 Clash 提供批量延迟接口：
//
//	GET /group/<组名>/delay?timeout=3000&url=http://www.gstatic.com/generate_204
//	→ {"🇭🇰 香港Y01":473, "🇯🇵 日本Y01":227, ...}
//
// 一次请求拿到全部节点的真实延迟，代价极低（探测由 Clash 自己发出，
// 不经过本网关、不消耗上游模型积分）。用它取代 TCP 探测作为健康判据。
//
// 延迟语义（Clash 约定）：0 = 探测失败/超时（不可用）；>0 = 可用（毫秒）。
package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DelayTestURL 延迟探测的目标地址（gstatic 204 是 Clash 社区常用探针：
// 返回空 204、无内容、极快）。
const DelayTestURL = "http://www.gstatic.com/generate_204"

// DelayTimeout 单节点延迟探测超时（毫秒，Clash 侧参数）。
const DelayTimeout = 3000

// FetchDelays 通过 Clash 的批量接口获取各节点真实延迟（毫秒）。
// group 为策略组名（本机实测可用 GLOBAL）；返回 节点名 → 延迟(ms)，0 表示不可用。
func FetchDelays(apiBase, secret, group string, timeoutMS int) (map[string]int, error) {
	if strings.TrimSpace(group) == "" {
		group = "GLOBAL"
	}
	if timeoutMS <= 0 {
		timeoutMS = DelayTimeout
	}
	u := fmt.Sprintf("%s/group/%s/delay?timeout=%d&url=%s",
		strings.TrimRight(apiBase, "/"), urlEscape(group), timeoutMS, urlEscape(DelayTestURL))
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	cli := &http.Client{Timeout: time.Duration(timeoutMS+2000) * time.Millisecond}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clash delay api status %d", resp.StatusCode)
	}
	out := map[string]int{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// urlEscape 百分号编码（节点名含 emoji/空格/竖线，必须编码）。
func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// DelayHealthy 报告某节点是否可用（延迟 > 0）。缺失的节点视为不可用。
func DelayHealthy(delays map[string]int, node string) bool {
	d, ok := delays[node]
	return ok && d > 0
}

// ApplyDelays 把延迟结果写入注册表健康状态：
// 延迟 > 0 → 健康；否则（0 或缺失）→ 不健康。
// 这比 TCP 探测准确：反映的是"出口节点能否真正出网"。
func (r *Registry) ApplyDelays(delays map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 缓存延迟供"按延迟选节点"与界面展示
	if r.delays == nil {
		r.delays = map[string]int{}
	}
	for k, v := range delays {
		r.delays[k] = v
	}
	for _, l := range r.listeners {
		if d, ok := delays[l.Node]; ok && d > 0 {
			delete(r.unhealthy, l.Port)
		} else {
			if _, seen := r.unhealthy[l.Port]; !seen {
				r.unhealthy[l.Port] = time.Now()
			}
		}
	}
}

// DelayOf 返回某节点最近一次探测到的延迟（毫秒）；0 = 未探测或不可用。
func (r *Registry) DelayOf(node string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.delays[node]
}

// NodeOption 界面用的"可选节点"（含健康状态与延迟）。
type NodeOption struct {
	Node    string
	Port    int
	Region  Region
	Delay   int // 毫秒；0 = 不可用/未探测
	Healthy bool
}

// NodeOptions 返回全部可选节点（按地区优先级 + 延迟升序）。
// 供界面让用户自己挑选节点。
func (r *Registry) NodeOptions() []NodeOption {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeOption, 0, len(r.listeners))
	for _, l := range r.listeners {
		healthy := true
		if _, bad := r.unhealthy[l.Port]; bad {
			healthy = false
		}
		out = append(out, NodeOption{
			Node:    l.Node,
			Port:    l.Port,
			Region:  l.Region,
			Delay:   r.delays[l.Node],
			Healthy: healthy,
		})
	}
	// 排序：可用的排前面；可用内部按延迟升序（用户挑节点时最关心"哪个快且能用"）
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Healthy != out[j].Healthy {
			return out[i].Healthy
		}
		if out[i].Healthy && out[i].Delay != out[j].Delay {
			return out[i].Delay < out[j].Delay
		}
		return out[i].Region < out[j].Region
	})
	return out
}

// PickMode 自动选节点的排序策略。
type PickMode int

const (
	// PickByRegion 地区优先（HK→TW→JP→其他），同地区内按端口序。
	// 适用于"看重出口地区可信度"的场景（默认）。
	PickByRegion PickMode = iota
	// PickByDelay 延迟优先（哪个快用哪个），延迟相同再比地区。
	// 适用于"看重速度"的场景。
	PickByDelay
)

// PickBestNode 按地区优先挑最优节点（默认策略）。
func (r *Registry) PickBestNode() (Listener, bool) {
	return r.PickBestNodeBy(PickByRegion)
}

// PickBestNodeBy 按指定策略挑最优节点。健康是硬门槛（不可用的绝不选）；
// 全部不可用时回落第一个并返回 ok=true —— 让调用方仍有候选可试，
// 而不是直接失败（与选号器的"全冷却兜底"同思路）。
func (r *Registry) PickBestNodeBy(mode PickMode) (Listener, bool) {
	r.mu.RLock()
	delays := r.delays
	r.mu.RUnlock()

	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.listeners) == 0 {
		return Listener{}, false
	}
	healthy := make([]Listener, 0, len(r.listeners))
	for _, l := range r.listeners {
		if _, bad := r.unhealthy[l.Port]; !bad {
			healthy = append(healthy, l)
		}
	}
	if len(healthy) == 0 {
		return r.listeners[0], true // 全不可用：回落
	}
	if mode == PickByDelay {
		best := healthy[0]
		for _, l := range healthy[1:] {
			bd, ld := delays[best.Node], delays[l.Node]
			// 延迟 0 表示未探测/不可用；已健康的节点延迟应 >0，
			// 但仍做防御：0 不参与比较（视为无穷大）。
			if bd == 0 {
				best = l
				continue
			}
			if ld == 0 {
				continue
			}
			if ld < bd {
				best = l
			}
		}
		return best, true
	}
	// 地区优先：先取最高优先地区，再在该地区内挑延迟最低的。
	//
	// 修正（实测缺陷）：原实现只取"排序后第一个健康节点"，等价于"地区+端口序"，
	// 会在香港有 35ms 节点时选中同地区 60ms 的节点 —— 同地区内当然该挑最快的。
	bestRegion := healthy[0].Region
	for _, l := range healthy {
		if l.Region < bestRegion {
			bestRegion = l.Region
		}
	}
	best, bestDelay := Listener{}, 1<<30
	for _, l := range healthy {
		if l.Region != bestRegion {
			continue
		}
		d := delays[l.Node]
		if d <= 0 {
			continue // 未探测到延迟：不作为最优候选
		}
		if d < bestDelay {
			best, bestDelay = l, d
		}
	}
	if best.Port != 0 {
		return best, true
	}
	// 该地区全部缺延迟数据：回落到排序后的第一个健康节点
	for _, l := range healthy {
		if l.Region == bestRegion {
			return l, true
		}
	}
	return healthy[0], true
}

// SetNodeForAccount 手动把账号绑定到指定节点。
// 返回 false 表示节点不存在（此时不改动现有绑定，避免静默改错）。
func (r *Registry) SetNodeForAccount(uid, node string) bool {
	if uid == "" || node == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := -1
	for i, l := range r.listeners {
		if l.Node == node {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	r.byUID[uid] = idx
	return true
}

// ClearNodeForAccount 解除账号的手动指定，交还自动分配：
// 按当前策略重新挑一个节点（优先地区、可用为准），并返回新绑定。
//
// 用途：用户手动指定后想"交还自动管理"时使用。
func (r *Registry) ClearNodeForAccount(uid string) (Listener, bool) {
	if uid == "" {
		return Listener{}, false
	}
	l, ok := r.PickBestNode()
	if !ok {
		return Listener{}, false
	}
	if !r.SetNodeForAccount(uid, l.Node) {
		return Listener{}, false
	}
	return l, true
}

// AccountOfNode 返回绑定了指定节点的账号数（界面显示"该节点服务几个账号"）。
func (r *Registry) LoadOfNode(node string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, idx := range r.byUID {
		if idx >= 0 && idx < len(r.listeners) && r.listeners[idx].Node == node {
			n++
		}
	}
	return n
}
