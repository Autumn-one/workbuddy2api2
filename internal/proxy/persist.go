// persist.go — 账号→节点绑定的持久化。
//
// 为什么必须持久化：同一账号的出口 IP 必须【稳定】——IP 频繁变化本身就是
// 异常特征，比"多账号共用一个 IP"更容易触发风控。若重启后重新按负载分配，
// 账号会换 IP。因此把绑定落盘，重启后原样恢复。
package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// bindingFile 落盘结构（uid → 节点名）。
//
// 存【节点名】而非端口：Clash 的节点顺序变化（订阅更新）会导致端口重排，
// 用节点名恢复更稳；端口由当前 listeners 表反查。
type bindingFile struct {
	Bindings map[string]string `json:"bindings"`
}

// LoadBindings 从文件恢复绑定；文件缺失/损坏静默降级为空（不影响启动）。
// 只会恢复"节点仍存在"的绑定；节点已消失的绑定被丢弃（下次按负载重新分配）。
func (r *Registry) LoadBindings(path string) {
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var bf bindingFile
	if json.Unmarshal(raw, &bf) != nil || len(bf.Bindings) == 0 {
		return
	}
	// 节点名 → listeners 下标（同名节点唯一）
	byNode := make(map[string]int, len(r.listeners))
	for i, l := range r.listeners {
		if _, dup := byNode[l.Node]; !dup {
			byNode[l.Node] = i
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for uid, node := range bf.Bindings {
		if idx, ok := byNode[node]; ok {
			r.byUID[uid] = idx
		}
	}
}

// SaveBindings 把当前绑定原子落盘（tmp + rename）。失败仅忽略——
// 绑定属运行态辅助信息，不应影响主流程。
func (r *Registry) SaveBindings(path string) {
	if path == "" {
		return
	}
	snap := r.Snapshot()
	bindings := make(map[string]string, len(snap))
	for _, b := range snap {
		bindings[b.UID] = b.Node
	}
	raw, err := json.MarshalIndent(bindingFile{Bindings: bindings}, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
