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

// ApplyDelays 把延迟结果写入注册表健康状态与延迟缓存，并自动换绑死节点上的账号。
//
// 健康判定：延迟 > 0 → 健康；否则（0 = 失败/超时）→ 不健康。
// 本函数是唯一有权判定"节点不可用"的入口（不再被 TCP 端口探测覆盖）。
//
// 自动换绑（用户要求）：绑在失败节点上的账号立即换到【可达且负载最少】的节点
// （**不比较延迟**——用户明确要求"只要可达，不计较快慢"），换绑后通过回调通知
// （GUI 落盘 + 日志）。全部节点失败时不换（防抖动把账号从一个死节点换到另一个死节点）；
// 账号本来就绑在健康节点上不动（稳定优先）。
//
// 注意：本函数只做"按本轮结果对齐健康状态"，不解决"节点恢复了要不要换回来"——
// 那由下一轮探测自动完成（健康节点被重新标记健康，但已换走的账号【不换回】，
// 出口 IP 稳定优先于"回到老节点"）。
func (r *Registry) ApplyDelays(delays map[string]int) {
	r.mu.Lock()
	// 缓存延迟**仅供界面展示**（不参与任何自动选节点/换绑决策）
	if r.delays == nil {
		r.delays = map[string]int{}
	}
	for k, v := range delays {
		r.delays[k] = v
	}
	// 先对齐健康状态
	healthyCount := 0
	for _, l := range r.listeners {
		if d, ok := delays[l.Node]; ok && d > 0 {
			delete(r.unhealthy, l.Port)
			healthyCount++
		} else {
			if _, seen := r.unhealthy[l.Port]; !seen {
				r.unhealthy[l.Port] = time.Now()
			}
		}
	}
	// 自动换绑：把绑在失败节点上的账号挪到可达的节点（按负载最少挑，不看延迟）。
	// 全部失败时不换——没有可用目标，换也是白换（且会引发连锁重分配）。
	var events []RebindEvent
	if healthyCount > 0 {
		for uid, idx := range r.byUID {
			if idx < 0 || idx >= len(r.listeners) {
				continue
			}
			cur := r.listeners[idx]
			if _, bad := r.unhealthy[cur.Port]; !bad {
				continue // 当前节点健康，不动（稳定优先）
			}
			if l, ok := r.rebindLocked(uid); ok {
				events = append(events, RebindEvent{UID: uid, FromNode: cur.Node, ToNode: l.Node, ToPort: l.Port})
			}
		}
	}
	cb := r.onAutoRebind
	r.mu.Unlock()
	// 回调在锁外调用，避免实现方阻塞注册表。
	if cb != nil {
		for _, ev := range events {
			cb(ev)
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

// ClearNodeForAccount 解除账号的手动指定，交还自动分配。
//
// 交还后按【可达 → 负载最少】重新分配（与首次分配同规则，**不看延迟**）。
// 旧实现是"地区优先挑第一个健康节点"，会让所有交还的账号再次挤到同一个节点上
// ——集中正是这个功能要避免的（实测 28/30 个账号挤在一个节点）。
//
// 用途：用户手动指定后想"交还自动管理"时使用。
func (r *Registry) ClearNodeForAccount(uid string) (Listener, bool) {
	if uid == "" {
		return Listener{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	best, ok := r.pickLeastLoadedLocked(r.loadLocked(), 0)
	if !ok {
		return Listener{}, false // 没有任何可达节点：不改动现有绑定
	}
	r.byUID[uid] = best
	return r.listeners[best], true
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
