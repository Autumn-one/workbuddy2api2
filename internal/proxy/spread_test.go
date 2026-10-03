package proxy

import "testing"

// ─────────────── 出口分散（用户要求：一账号一出口 IP，只要可达、不计较快慢） ───────────────
//
// 实测问题（2026-10-01）：data/proxy-bindings.json 里 28/30 个账号挤在同一个节点
// （🇭🇰 香港Y05）。成因是换绑曾按"延迟最低"挑目标，一次批量节点失效就把所有账号
// 换到同一个最快节点上；而绑定又是"稳定优先、永不回迁"，集中状态被永久固化。

// TestRebindSpreadsAccountsAcrossNodes 批量失效时，死节点上的账号必须被摊开，
// 而不是全挤到延迟最低的那个节点上。
func TestRebindSpreadsAccountsAcrossNodes(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "dead", Port: 34567, Node: "死节点", Region: RegionHK},
		{Name: "a", Port: 34568, Node: "香港A", Region: RegionHK},
		{Name: "b", Port: 34569, Node: "香港B", Region: RegionHK},
		{Name: "c", Port: 34570, Node: "香港C", Region: RegionHK},
	})
	// 三个账号都绑在死节点上（模拟批量失效前的集中状态）
	for _, uid := range []string{"u1", "u2", "u3"} {
		if !r.Adopt(uid, "死节点") {
			t.Fatalf("Adopt %s 失败", uid)
		}
	}
	// 延迟差异故意很大：香港B 最快——但选路不看延迟
	r.ApplyDelays(map[string]int{"死节点": 0, "香港A": 500, "香港B": 30, "香港C": 300})

	nodes := map[string]int{}
	for _, uid := range []string{"u1", "u2", "u3"} {
		nodes[r.NodeFor(uid)]++
	}
	if len(nodes) != 3 {
		t.Fatalf("三个账号应摊到三个不同节点（不得都去最快的香港B）, got %v", nodes)
	}
	for node, c := range nodes {
		if c != 1 {
			t.Errorf("节点 %s 上有 %d 个账号（应各 1 个）: %v", node, c, nodes)
		}
		if node == "死节点" {
			t.Errorf("账号不应留在死节点: %v", nodes)
		}
	}
}

// TestRespreadConcentratedBindings 「重新分散」把 28/30 那种集中状态摊开：
// 每个节点留一个账号，其余挪到空节点上。
func TestRespreadConcentratedBindings(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "y05", Port: 34571, Node: "香港Y05", Region: RegionHK},
		{Name: "y01", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "y02", Port: 34568, Node: "香港Y02", Region: RegionHK},
		{Name: "y03", Port: 34569, Node: "香港Y03", Region: RegionHK},
		{Name: "y04", Port: 34570, Node: "香港Y04", Region: RegionHK},
		{Name: "y06", Port: 34572, Node: "香港Y06", Region: RegionHK},
	})
	uids := []string{"u1", "u2", "u3", "u4", "u5", "u6"}
	for _, uid := range uids {
		if !r.Adopt(uid, "香港Y05") {
			t.Fatalf("Adopt %s 失败", uid)
		}
	}
	if max, total := r.Concentration(); max != 6 || total != 6 {
		t.Fatalf("前置条件：应 6/6 全挤在一个节点, got %d/%d", max, total)
	}

	if moved := r.Respread(); moved != 5 {
		t.Errorf("6 个账号 / 6 个节点应移动 5 个（每个节点留一个）, got %d", moved)
	}
	nodes := map[string]int{}
	for _, uid := range uids {
		nodes[r.NodeFor(uid)]++
	}
	if len(nodes) != 6 {
		t.Errorf("分散后应各占一个节点, got %v", nodes)
	}
	if max, _ := r.Concentration(); max != 1 {
		t.Errorf("分散后最忙节点应只有 1 个账号, got %d", max)
	}
}

// TestRespreadNoopWhenSpread 本来就分散时不动任何账号（出口 IP 稳定优先）。
func TestRespreadNoopWhenSpread(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "香港Y02", Region: RegionHK},
	})
	r.Adopt("u1", "香港Y01")
	r.Adopt("u2", "香港Y02")
	if moved := r.Respread(); moved != 0 {
		t.Errorf("本来就分散时不应移动任何账号, got %d", moved)
	}
}

// TestRespreadSkipsUnhealthyTargets 只往可达节点摊（不可达节点不参与）。
func TestRespreadSkipsUnhealthyTargets(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "y05", Port: 34571, Node: "香港Y05", Region: RegionHK},
		{Name: "dead", Port: 34567, Node: "死节点", Region: RegionHK},
		{Name: "alive", Port: 34568, Node: "可用节点", Region: RegionHK},
	})
	r.Adopt("u1", "香港Y05")
	r.Adopt("u2", "香港Y05")
	r.ApplyDelays(map[string]int{"香港Y05": 50, "死节点": 0, "可用节点": 80})

	r.Respread()
	if got := r.NodeFor("u2"); got != "可用节点" && got != "香港Y05" {
		t.Errorf("只应摊到可达节点, got %q", got)
	}
	for _, uid := range []string{"u1", "u2"} {
		if r.NodeFor(uid) == "死节点" {
			t.Errorf("%s 被分到了不可达节点", uid)
		}
	}
}

// TestAutoRespreadThreshold 启动时的自动分散要有阈值：阈值内不动（可能是用户手动指定），
// 明显扎堆（≥minShare）才摊开。
//
// 回归点（实测 2026-10-02）：绑定持久化会把历史集中状态原样继承，用户重启后依然
// "只连一个节点"——所以启动时必须自动收拾明显扎堆的情况。
func TestAutoRespreadThreshold(t *testing.T) {
	// 2 个账号共用一节点 → 阈值内，不动
	small := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港A", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "香港B", Region: RegionHK},
	})
	small.Adopt("u1", "香港A")
	small.Adopt("u2", "香港A")
	if moved, max, total := small.AutoRespread(3); moved != 0 || max != 2 || total != 2 {
		t.Errorf("阈值内（2 个共用）不该动手: moved=%d max=%d total=%d", moved, max, total)
	}
	if got := small.NodeFor("u2"); got != "香港A" {
		t.Errorf("阈值内不应改绑定, got %q", got)
	}

	// 3 个账号共用一节点 → 摊开
	big := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港A", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "香港B", Region: RegionHK},
		{Name: "c", Port: 34569, Node: "香港C", Region: RegionHK},
	})
	for _, uid := range []string{"u1", "u2", "u3"} {
		big.Adopt(uid, "香港A")
	}
	moved, max, total := big.AutoRespread(3)
	if moved != 2 || max != 3 || total != 3 {
		t.Errorf("3 个共用应摊开 2 个: moved=%d max=%d total=%d", moved, max, total)
	}
	if m, _ := big.Concentration(); m != 1 {
		t.Errorf("摊开后最忙节点应为 1 个账号, got %d", m)
	}

	// 只有 1 个节点可用（没有空闲节点）→ 不动，也不报错
	only := NewRegistry([]Listener{{Name: "a", Port: 34567, Node: "香港A", Region: RegionHK}})
	for _, uid := range []string{"u1", "u2", "u3"} {
		only.Adopt(uid, "香港A")
	}
	if moved, max, _ := only.AutoRespread(3); moved != 0 || max != 3 {
		t.Errorf("无空闲节点时不该硬挪: moved=%d max=%d", moved, max)
	}
}

// TestConcentrationReportsBusiestNode 集中度：最忙节点上的账号数 / 绑定总数。
func TestConcentrationReportsBusiestNode(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "香港Y02", Region: RegionHK},
	})
	r.Adopt("u1", "香港Y01")
	r.Adopt("u2", "香港Y01")
	r.Adopt("u3", "香港Y02")
	if max, total := r.Concentration(); max != 2 || total != 3 {
		t.Errorf("集中度应 2/3, got %d/%d", max, total)
	}
}
