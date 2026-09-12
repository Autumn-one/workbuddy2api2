package proxy

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// ─────────────── 账号级代理分配 ───────────────
//
// 需求（用户）：给每个账号分配一个独立出口 IP，降低"同 IP 多账号"的风控特征。
//
// 实现方式：本地 Clash 用 listeners 给每个节点开一个专属端口
// （如 34567 → 香港Y01），网关按"账号 → 端口"映射发请求。
// 这样不依赖 Clash API（切节点是全局状态，并发会互相踩），只需固定端口。
//
// 关键约束：
//  1. 同一账号必须稳定走同一节点（IP 频繁变化本身就是异常特征）；
//  2. 节点不通时要能换到"通的"节点（用户要求：定期低成本探测）；
//  3. 节点数不足时允许一个节点对应多个账号（用户明确允许）；
//  4. 优先使用 HK / TW / JP 节点（用户指定排序）。

// TestNewRegistryFromListeners 从 listener 列表构建注册表：端口解析、名称保留。
func TestNewRegistryFromListeners(t *testing.T) {
	ls := []Listener{
		{Name: "acct-01-HK", Port: 34567, Node: "🇭🇰 香港Y01", Region: RegionHK},
		{Name: "acct-02-JP", Port: 34568, Node: "🇯🇵 日本Y01", Region: RegionJP},
	}
	r := NewRegistry(ls)
	if len(r.Listeners()) != 2 {
		t.Fatalf("listener 数=%d want 2", len(r.Listeners()))
	}
	if got := r.Listeners()[0].Node; got != "🇭🇰 香港Y01" {
		t.Errorf("节点名应原样保留（界面要显示可读名）: %q", got)
	}
	if got := r.Listeners()[0].ProxyURL(); got != "http://127.0.0.1:34567" {
		t.Errorf("ProxyURL=%q want http://127.0.0.1:34567", got)
	}
}

// TestAssignStableByUID 同一账号多次分配必须落到同一 listener（IP 稳定）。
func TestAssignStableByUID(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
		{Name: "c", Port: 34569, Node: "美国Y01", Region: RegionOther},
	})
	first, ok := r.Assign("uid-abc")
	if !ok {
		t.Fatal("分配失败")
	}
	for i := 0; i < 20; i++ {
		again, _ := r.Assign("uid-abc")
		if again.Port != first.Port {
			t.Fatalf("同一账号分配不稳定: %d → %d", first.Port, again.Port)
		}
	}
}

// TestAssignPrefersPriorityRegions 优先使用 HK/TW/JP：账号数少于优先节点数时
// 不应分配到"其他"地区节点。
func TestAssignPrefersPriorityRegions(t *testing.T) {
	var ls []Listener
	// 3 个优先节点 + 3 个其他地区
	for i := 0; i < 3; i++ {
		ls = append(ls, Listener{Name: "hk", Port: 34567 + i, Node: "香港", Region: RegionHK})
	}
	for i := 0; i < 3; i++ {
		ls = append(ls, Listener{Name: "us", Port: 34570 + i, Node: "美国", Region: RegionOther})
	}
	r := NewRegistry(ls)
	for _, uid := range []string{"u1", "u2", "u3"} {
		l, ok := r.Assign(uid)
		if !ok {
			t.Fatalf("%s 分配失败", uid)
		}
		if l.Region == RegionOther {
			t.Errorf("%s 应优先分配 HK/TW/JP，却得到 %q", uid, l.Node)
		}
	}
}

// TestAssignReuseWhenNotEnough 节点不足时允许一个节点服务多个账号（用户允许）。
func TestAssignReuseWhenNotEnough(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "only", Port: 34567, Node: "香港Y01", Region: RegionHK},
	})
	a1, ok1 := r.Assign("u1")
	a2, ok2 := r.Assign("u2")
	if !ok1 || !ok2 {
		t.Fatal("节点不足时应复用而非失败")
	}
	if a1.Port != a2.Port {
		t.Errorf("只有一个节点时应复用同一端口: %d vs %d", a1.Port, a2.Port)
	}
}

// TestAssignEmptyRegistry 无可用节点时返回 ok=false（调用方回落直连）。
func TestAssignEmptyRegistry(t *testing.T) {
	r := NewRegistry(nil)
	if _, ok := r.Assign("u1"); ok {
		t.Error("无节点应返回 ok=false")
	}
}

// TestAssignAvoidsUnhealthy 优先分配健康节点；不健康节点在健康节点够用时不被选中。
func TestAssignAvoidsUnhealthy(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "bad", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "good", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	r.MarkUnhealthy(34567)
	for _, uid := range []string{"u1", "u2", "u3"} {
		l, ok := r.Assign(uid)
		if !ok {
			t.Fatalf("%s 分配失败", uid)
		}
		if l.Port == 34567 {
			t.Errorf("%s 不应分配到已标记不健康的节点", uid)
		}
	}
}

// TestAssignFallsBackWhenAllUnhealthy 全部不健康时仍应返回一个（好过直接失败）。
func TestAssignFallsBackWhenAllUnhealthy(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	r.MarkUnhealthy(34567)
	r.MarkUnhealthy(34568)
	if _, ok := r.Assign("u1"); !ok {
		t.Error("全部不健康时仍应返回候选（回落），而不是失败")
	}
}

// TestRebindMovesAccountToHealthyNode 账号绑定的节点不通 → 换到健康节点（用户要求）。
func TestRebindMovesAccountToHealthyNode(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "bad", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "good", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	orig, _ := r.Assign("u1")
	if orig.Port != 34567 {
		t.Fatalf("前置条件：u1 应分到第一个节点, got %d", orig.Port)
	}
	r.MarkUnhealthy(34567)
	moved, ok := r.Rebind("u1")
	if !ok {
		t.Fatal("重新绑定失败")
	}
	if moved.Port == 34567 {
		t.Error("重新绑定应换到健康节点")
	}
	// 重绑后必须稳定（后续查询返回新节点）
	for i := 0; i < 5; i++ {
		cur, _ := r.Assign("u1")
		if cur.Port != moved.Port {
			t.Fatalf("重绑后应保持稳定: %d vs %d", cur.Port, moved.Port)
		}
	}
}

// TestUnassignClearsBinding 解绑后不再占用该节点（用于账号删除）。
func TestUnassignClearsBinding(t *testing.T) {
	r := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}})
	r.Assign("u1")
	r.Unassign("u1")
	// 解绑后重新分配仍可用（不 panic、能拿到节点）
	if _, ok := r.Assign("u1"); !ok {
		t.Error("解绑后应可重新分配")
	}
}

// TestBindingSnapshotForUI 界面需要看到"哪个账号在用哪个节点"（可读节点名 + 端口）。
func TestBindingSnapshotForUI(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "🇭🇰 香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "🇯🇵 日本Y01", Region: RegionJP},
	})
	r.Assign("u1")
	r.Assign("u2")
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("快照应含 2 个账号, got %d", len(snap))
	}
	for _, b := range snap {
		if b.Node == "" || b.Port == 0 {
			t.Errorf("快照必须含可读节点名与端口: %+v", b)
		}
		if strings.HasPrefix(b.Node, "http") {
			t.Errorf("节点名不应是 URL（界面要可读名）: %q", b.Node)
		}
	}
}

// TestListenerProxyURL 端口 → 代理 URL（HTTP 代理，Go 原生支持）。
func TestListenerProxyURL(t *testing.T) {
	l := Listener{Name: "x", Port: 34567, Node: "香港"}
	u, err := url.Parse(l.ProxyURL())
	if err != nil {
		t.Fatalf("ProxyURL 非法: %v", err)
	}
	if u.Scheme != "http" || u.Host != "127.0.0.1:34567" {
		t.Errorf("ProxyURL=%v", u)
	}
}

// TestRegionFromNodeName 从节点名识别地区（HK/TW/JP 优先排序的依据）。
func TestRegionFromNodeName(t *testing.T) {
	cases := []struct {
		node string
		want Region
	}{
		{"🇭🇰 香港Y01", RegionHK},
		{"香港Y02 | IEPL", RegionHK},
		{"🇨🇳 台湾Y01 | IEPL | x2", RegionTW},
		{"🇯🇵 日本Y01", RegionJP},
		{"🇸🇬 新加坡Y01", RegionOther},
		{"🇺🇸 美国Y01", RegionOther},
	}
	for _, c := range cases {
		if got := RegionFromNodeName(c.node); got != c.want {
			t.Errorf("RegionFromNodeName(%q)=%v want %v", c.node, got, c.want)
		}
	}
}

// TestHealthCheckIntervalPositive 健康检查周期必须为正且"低成本"
// （用户要求：定期以低成本方式检测节点是否通）。
func TestHealthCheckIntervalPositive(t *testing.T) {
	if DefaultHealthInterval <= 0 {
		t.Fatalf("DefaultHealthInterval=%v 必须为正", DefaultHealthInterval)
	}
	if DefaultHealthInterval < time.Minute {
		t.Errorf("周期 %v 过短，会增加无谓开销", DefaultHealthInterval)
	}
}
