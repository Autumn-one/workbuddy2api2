package proxy

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// ─────────────── 代理绑定列表的完整性与可操作性 ───────────────
//
// 用户反馈（两次设计错误）：
//  1. 开关打开后列表【默认空白】——因为 Snapshot 只返回"已分配"的账号，
//     而分配是懒执行的（要等请求打到该账号才 Assign）。用户期望：一开启就
//     看到全部账号各自绑到哪个节点。
//  2. 「切换选中账号节点」按钮读的是【账号页】的选中行，却放在代理页——
//     点了会"凭空冒出一行"（给那个账号新分配节点）。切换应作用于代理列表自身。

// TestEnsureAllAssigned 一次性为所有账号建立绑定（开关后立刻填满列表）。
func TestEnsureAllAssigned(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
		{Name: "c", Port: 34569, Node: "美国Y01", Region: RegionOther},
	})
	uids := []string{"u1", "u2", "u3", "u4", "u5"}
	r.EnsureAllAssigned(uids)

	snap := r.Snapshot()
	if len(snap) != len(uids) {
		t.Fatalf("应立刻绑定全部 %d 个账号, got %d", len(uids), len(snap))
	}
	// 每个账号都必须有节点名与端口（界面要显示）
	for _, b := range snap {
		if b.Node == "" || b.Port == 0 {
			t.Errorf("绑定不完整: %+v", b)
		}
	}
}

// TestEnsureAllAssignedKeepsExisting 重复调用不得改动已有绑定（IP 稳定性）。
func TestEnsureAllAssignedKeepsExisting(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	uids := []string{"u1", "u2", "u3"}
	r.EnsureAllAssigned(uids)
	before := map[string]string{}
	for _, b := range r.Snapshot() {
		before[b.UID] = b.Node
	}
	// 再来一次（模拟刷新）
	r.EnsureAllAssigned(uids)
	for _, b := range r.Snapshot() {
		if before[b.UID] != b.Node {
			t.Errorf("%s 的节点被改动: %q → %q（必须稳定）", b.UID, before[b.UID], b.Node)
		}
	}
}

// TestEnsureAllAssignedSkipsEmptyUID 空 uid 忽略（不产生垃圾行）。
func TestEnsureAllAssignedSkipsEmptyUID(t *testing.T) {
	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	r.EnsureAllAssigned([]string{"u1", "", "u2"})
	if n := len(r.Snapshot()); n != 2 {
		t.Errorf("应只绑定 2 个有效账号, got %d", n)
	}
}

// TestEnsureAllAssignedEmptyPool 无节点时安全（不 panic）。
func TestEnsureAllAssignedEmptyPool(t *testing.T) {
	r := NewRegistry(nil)
	r.EnsureAllAssigned([]string{"u1", "u2"})
	if n := len(r.Snapshot()); n != 0 {
		t.Errorf("无节点时不应产生绑定, got %d", n)
	}
}

// TestRowsForAccounts 界面需要"账号列表 × 当前绑定"的完整视图：
// 即使某账号尚未绑定（理论上不该发生，但列表要健壮）也要给出一行。
func TestRowsForAccounts(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	// 只手动绑定 u1，u2 未绑定
	r.Assign("u1")
	// 用 Registry 提供的查询方法：未绑定的返回空节点（界面显示"未分配"）
	if n := r.NodeFor("u2"); n != "" {
		t.Errorf("未绑定账号应返回空节点, got %q", n)
	}
	if n := r.NodeFor("u1"); n == "" {
		t.Error("已绑定账号应有节点")
	}
}

// TestAuthImportNotNeeded 占位：确保本文件与 auth 无关（仅防误引）。
var _ = auth.Auth{}
