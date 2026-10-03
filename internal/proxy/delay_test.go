package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ─────────────── 真实连通检测（节点延迟）───────────────
//
// 背景（用户问"隔多久检测、立即探测是什么意思"）：原检测只做【TCP 连通】，
// 只能证明"Clash 在监听端口"，不能证明出口节点真能通外网
// （Clash 可能接受连接但节点已挂）。
//
// 实测发现 Clash 提供批量延迟接口：
//   GET /group/<组名>/delay?timeout=3000&url=http://www.gstatic.com/generate_204
//   → {"🇭🇰 香港Y01":473, "🇯🇵 日本Y01":227, ...}
// 一次请求即可拿到全部节点的真实延迟，代价极低（Clash 自己发探测请求，
// 不经过本网关、不消耗上游模型积分）。
//
// 用它替代/补充 TCP 探测，让"连通"列反映真实可用性。

// fakeClashDelay 起一个假的 Clash，提供 /proxies 与 /group/<g>/delay。
func fakeClashDelay(t *testing.T, delays map[string]int, group string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxies" {
			body := map[string]any{"proxies": map[string]any{}}
			pm := body["proxies"].(map[string]any)
			for name := range delays {
				pm[name] = map[string]any{"type": "Shadowsocks"}
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		if len(r.URL.Path) > 7 && r.URL.Path[:7] == "/group/" {
			_ = json.NewEncoder(w).Encode(delays)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchDelays 解析批量延迟结果。
func TestFetchDelays(t *testing.T) {
	srv := fakeClashDelay(t, map[string]int{
		"🇭🇰 香港Y01": 473,
		"🇯🇵 日本Y01": 36,
		"🇺🇸 美国Y01": 0, // 0 = 超时/不可用
	}, "GLOBAL")
	got, err := FetchDelays(srv.URL, "", "GLOBAL", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 个节点的延迟, got %d", len(got))
	}
	if got["🇭🇰 香港Y01"] != 473 {
		t.Errorf("香港延迟=%d want 473", got["🇭🇰 香港Y01"])
	}
	// 延迟 0 表示该节点不可用（Clash 用 0 表达超时/失败）
	if got["🇺🇸 美国Y01"] != 0 {
		t.Errorf("美国应为 0（不可用）, got %d", got["🇺🇸 美国Y01"])
	}
}

// TestDelayHealthy 延迟 > 0 视为可用；0 或缺失视为不可用。
func TestDelayHealthy(t *testing.T) {
	delays := map[string]int{"good": 100, "dead": 0}
	if !DelayHealthy(delays, "good") {
		t.Error("延迟>0 应视为可用")
	}
	if DelayHealthy(delays, "dead") {
		t.Error("延迟=0 应视为不可用")
	}
	if DelayHealthy(delays, "missing") {
		t.Error("缺失的节点应视为不可用")
	}
}

// TestApplyDelaysToRegistry 把延迟结果写入注册表健康状态。
func TestApplyDelaysToRegistry(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
		{Name: "c", Port: 34569, Node: "美国Y01", Region: RegionOther},
	})
	delays := map[string]int{"香港Y01": 473, "日本Y01": 0} // 日本不可用；美国缺失
	r.ApplyDelays(delays)

	if !r.Healthy(34567) {
		t.Error("香港（延迟473）应健康")
	}
	if r.Healthy(34568) {
		t.Error("日本（延迟0）应不健康")
	}
	if r.Healthy(34569) {
		t.Error("美国（延迟缺失）应不健康")
	}
}

// TestAutoAssignIgnoresDelay 自动分配只看"可达 + 负载最少"，**不看延迟**。
//
// 用户要求（2026-10-02）：只要可达就行，不计较快慢——延迟不得参与任何自动选路。
// 回归点：曾按"延迟最低"挑节点，一次批量节点失效把 28/30 个账号挤到同一个最快
// 节点上，出口 IP 反而比失效前更集中。
func TestAutoAssignIgnoresDelay(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "hk1", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "hk2", Port: 34568, Node: "香港Y02", Region: RegionHK},
		{Name: "jp1", Port: 34569, Node: "日本Y01", Region: RegionJP},
	})
	// 日本 20ms 最快、香港Y02 次之；但自动分配不认延迟。
	r.ApplyDelays(map[string]int{"香港Y01": 800, "香港Y02": 120, "日本Y01": 20})

	got := map[string]int{}
	for _, uid := range []string{"u1", "u2", "u3"} {
		l, ok := r.Assign(uid)
		if !ok {
			t.Fatal("应能分配")
		}
		got[l.Node]++
	}
	if len(got) != 3 {
		t.Errorf("三个账号应摊到三个不同节点（负载最少优先，不是都去最快的日本Y01）: %v", got)
	}
	for node, n := range got {
		if n != 1 {
			t.Errorf("节点 %s 上有 %d 个账号（应各 1 个）: %v", node, n, got)
		}
	}
}

// TestSetNodeForAccount 手动把账号指定到某节点（用户要求：可自己选）。
func TestSetNodeForAccount(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	r.Assign("u1") // 默认分到第一个
	if r.NodeFor("u1") != "香港Y01" {
		t.Fatalf("前置条件：应分到香港, got %q", r.NodeFor("u1"))
	}
	// 手动指定到日本
	if ok := r.SetNodeForAccount("u1", "日本Y01"); !ok {
		t.Fatal("手动指定应成功")
	}
	if r.NodeFor("u1") != "日本Y01" {
		t.Errorf("手动指定后应为日本Y01, got %q", r.NodeFor("u1"))
	}
	// 指定不存在的节点应失败（不静默改错）
	if r.SetNodeForAccount("u1", "不存在节点") {
		t.Error("指定不存在的节点应返回 false")
	}
	if r.NodeFor("u1") != "日本Y01" {
		t.Error("失败时不应改动现有绑定")
	}
}

// TestListNodeOptions 界面需要"可选节点列表"（供用户挑选）。
func TestListNodeOptions(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	r.ApplyDelays(map[string]int{"香港Y01": 473, "日本Y01": 0})
	opts := r.NodeOptions()
	if len(opts) != 2 {
		t.Fatalf("应 2 个可选节点, got %d", len(opts))
	}
	// 每项要有可读节点名与健康状态（延迟 0 的标为不可用）
	for _, o := range opts {
		if o.Node == "" {
			t.Errorf("节点名不得为空: %+v", o)
		}
	}
	if opts[0].Healthy != true || opts[1].Healthy != false {
		t.Errorf("健康状态错误: %+v", opts)
	}
}

// TestClearNodeForAccountReturnsToAuto 手动指定后可交还自动分配。
//
// 交还规则与首次分配一致：**可达 → 负载最少**（不看延迟）。
// 此处香港空着、日本被自己占着 → 交还后应落到香港。
func TestClearNodeForAccountReturnsToAuto(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "hk", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "jp", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	r.ApplyDelays(map[string]int{"香港Y01": 500, "日本Y01": 50})
	// 手动指定到日本
	r.SetNodeForAccount("u1", "日本Y01")
	if r.NodeFor("u1") != "日本Y01" {
		t.Fatal("前置条件：应指定到日本")
	}
	// 交还自动 → 按负载最少（香港空着、日本被占）应落到香港
	l, ok := r.ClearNodeForAccount("u1")
	if !ok {
		t.Fatal("交还自动应成功")
	}
	if l.Node != "香港Y01" {
		t.Errorf("自动分配应选负载最少的香港Y01（不看延迟）, got %q", l.Node)
	}
	if r.NodeFor("u1") != "香港Y01" {
		t.Errorf("绑定未更新: %q", r.NodeFor("u1"))
	}
}

// TestSetNodeDoesNotAffectOthers 手动指定某个账号不影响其他账号。
func TestSetNodeDoesNotAffectOthers(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: RegionJP},
	})
	r.EnsureAllAssigned([]string{"u1", "u2", "u3"})
	before := map[string]string{}
	for _, b := range r.Snapshot() {
		before[b.UID] = b.Node
	}
	r.SetNodeForAccount("u1", "日本Y01")
	for _, b := range r.Snapshot() {
		if b.UID == "u1" {
			continue
		}
		if b.Node != before[b.UID] {
			t.Errorf("指定 u1 不应影响 %s: %q → %q", b.UID, before[b.UID], b.Node)
		}
	}
}

// TestLoadOfNode 统计节点负载（界面显示"该节点服务几个账号"）。
func TestLoadOfNode(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
	})
	r.EnsureAllAssigned([]string{"u1", "u2", "u3"})
	if n := r.LoadOfNode("香港Y01"); n != 3 {
		t.Errorf("香港Y01 应服务 3 个账号, got %d", n)
	}
	if n := r.LoadOfNode("不存在"); n != 0 {
		t.Errorf("不存在的节点负载应为 0, got %d", n)
	}
}

// TestDelayConstantsSane 探测参数合理性。
func TestDelayConstantsSane(t *testing.T) {
	if DelayTimeout <= 0 || DelayTimeout > 10000 {
		t.Errorf("DelayTimeout=%d 不合理", DelayTimeout)
	}
	if DelayTestURL == "" {
		t.Error("探测目标 URL 不得为空")
	}
}

// TestPickBestNode* 系列已删除：自动选路不再按延迟挑（用户要求"只要可达，不计较快慢"），
// 相关回归由 TestAutoAssignIgnoresDelay 与 spread_test.go 覆盖。
