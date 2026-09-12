// persist.go — 账号→节点绑定 与 接管等级 的持久化。
//
// 为什么必须持久化：
//  1. 同一账号的出口 IP 必须【稳定】——IP 频繁变化本身就是异常特征，
//     比"多账号共用一个 IP"更容易触发风控。若重启后重新按负载分配，账号会换 IP。
//  2. 接管等级也要记住：一旦确认过"用户自己已经配好 listeners"，
//     之后启动就不该再去改写他的 Clash 配置。
package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// bindingFile 落盘结构。
//
// 关键设计：绑定存【节点名】而非端口——Clash 的节点顺序变化（订阅更新）会导致
// 端口重排，用节点名恢复更稳；端口由当前 listeners 表反查。
//
// 另外单独记一份【端口 → 节点】（listen_owner）：只有它能回答
// "这些端口是不是用户自己配置的"——查 clash-verge.yaml 也不行，
// 因为网关注入的 listeners 也是写在那里的。缺失该段（旧格式文件）时
// 一律按"端口是网关配的"处理，保证既有行为不变。
type bindingFile struct {
	Bindings map[string]string `json:"bindings"`
	// ListenOwner 端口 → 节点：上次落盘时已知的 listener 归属。
	// 取值约定："gateway" = 本程序配置的（可以安全移除/改写）；
	// 其他/缺失 = 用户自己配置的（绝不触碰）。
	ListenOwner map[string]string `json:"listen_owner,omitempty"`
}

// OwnerGateway 端口归属：本程序配置。
const OwnerGateway = "gateway"

// LoadBindings 从文件恢复绑定；文件缺失/损坏静默降级为空（不影响启动）。
// 只会恢复"节点仍存在"的绑定；节点已消失的绑定被丢弃（下次按负载重新分配）。
func (r *Registry) LoadBindings(path string) {
	bf, ok := readBindingFile(path)
	if !ok || len(bf.Bindings) == 0 {
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

// LoadBindingsReport 与 LoadBindings 相同，但额外返回"哪些绑定失效了"，
// 供启动时如实告知用户（例如订阅更新导致节点改名，只有受影响的账号被重分配）。
//
// 返回：
//
//	restored 成功继承上次对应关系的账号数
//	lost     上次有绑定、但节点已不存在（本次需重新分配）的账号数
func (r *Registry) LoadBindingsReport(path string) (restored, lost int) {
	bf, ok := readBindingFile(path)
	if !ok || len(bf.Bindings) == 0 {
		return 0, 0
	}
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
			restored++
			continue
		}
		lost++
	}
	return restored, lost
}

// readBindingFile 读取并解析绑定文件（不存在/损坏 → ok=false）。
func readBindingFile(path string) (bindingFile, bool) {
	var bf bindingFile
	if path == "" {
		return bf, false
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return bf, false
	}
	if json.Unmarshal(raw, &bf) != nil {
		return bf, false
	}
	return bf, true
}

// SaveBindings 把当前绑定原子落盘（tmp + rename）。失败仅忽略——
// 绑定属运行态辅助信息，不应影响主流程。
//
// 注意：本方法【不写 listen_owner】——它只更新账号绑定，不认识端口归属。
// 端口归属由 SaveBindingsWithOwner 写入，避免"随手保存绑定就把归属信息清空"。
func (r *Registry) SaveBindings(path string) {
	r.saveBindings(path, nil)
}

// SaveBindingsWithOwner 在保存绑定之外，同时记录"这些端口是谁配的"。
// 由开启代理的流程调用（那里才知道端口是本程序刚注入的、还是用户既有配置）。
func (r *Registry) SaveBindingsWithOwner(path string, owner map[int]string) {
	r.saveBindings(path, owner)
}

func (r *Registry) saveBindings(path string, owner map[int]string) {
	if path == "" {
		return
	}
	// 保留既有 listen_owner：只有显式传入 owner 时才重写。
	prev, _ := readBindingFile(path)
	bf := bindingFile{Bindings: map[string]string{}, ListenOwner: prev.ListenOwner}
	if owner != nil {
		bf.ListenOwner = make(map[string]string, len(owner))
		for port, who := range owner {
			bf.ListenOwner[itoa(port)] = who
		}
	}
	// 【合并而非覆盖】先把上次落盘的对应关系原样带上。
	//
	// 为什么必须这样（实测缺陷，2026-09-13）：保存时只写"当前账号池里的账号"，
	// 会让【暂时不在 auth 目录里】的账号丢掉它的对应关系（例如切到别的配置目录、
	// 账号文件临时移走）。等它回来时就会拿到一个全新的节点 —— 出口 IP 变了，
	// 而 IP 稳定正是这套机制存在的理由。
	for uid, node := range prev.Bindings {
		bf.Bindings[uid] = node
	}
	// 删除过（Unassign，例如账号被移除）的账号不再写回，避免"删了又复活"。
	r.mu.RLock()
	for uid := range r.removed {
		delete(bf.Bindings, uid)
	}
	r.mu.RUnlock()
	snap := r.Snapshot()
	for _, b := range snap {
		bf.Bindings[b.UID] = b.Node
	}
	raw, err := json.MarshalIndent(bf, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if dir != "" {
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

// readFileIfExists 读取文件内容（不存在 → 空 + error）。供 level.go 复用。
func readFileIfExists(path string) ([]byte, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(path)
}

// writeFileAtomic 原子写文件（tmp + rename），失败仅忽略。
func writeFileAtomic(path string, raw []byte) {
	if path == "" {
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

// LoadListenOwner 读取"端口 → 归属"记录（端口号作为键）。
// 缺失/损坏 → 空表；调用方据此按"端口是网关配置的"处理（保守假设：
// 宁可多清理自己配的端口，也不要因为信息缺失而不去移除自己上一轮写的 listeners）。
func LoadListenOwner(path string) map[int]string {
	out := map[int]string{}
	bf, ok := readBindingFile(path)
	if !ok {
		return out
	}
	for k, v := range bf.ListenOwner {
		if n, err := atoiStrict(k); err == nil {
			out[n] = v
		}
	}
	return out
}

// atoiStrict 严格解析十进制端口号。
func atoiStrict(s string) (int, error) {
	if s == "" {
		return 0, os.ErrInvalid
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
