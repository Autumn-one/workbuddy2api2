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

// TestPickBestNode 自动挑最优节点：优先 HK/TW/JP 且延迟最低。
func TestPickBestNode(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "hk-slow", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "hk-fast", Port: 34568, Node: "香港Y02", Region: RegionHK},
		{Name: "jp-fast", Port: 34569, Node: "日本Y01", Region: RegionJP},
		{Name: "us-fastest", Port: 34570, Node: "美国Y01", Region: RegionOther},
	})
	delays := map[string]int{
		"香港Y01": 800, "香港Y02": 120, "日本Y01": 90, "美国Y01": 20,
	}
	r.ApplyDelays(delays)
	// 策略一：地区优先（默认）→ 先取最高优先地区（HK），再在其中挑延迟最低的。
	// 香港Y02(120ms) 比香港Y01(800ms) 快，且美国虽 20ms 但地区优先级最低。
	best, ok := r.PickBestNode()
	if !ok {
		t.Fatal("应能挑出节点")
	}
	if best.Node != "香港Y02" {
		t.Errorf("地区优先应在香港里挑延迟最低的(香港Y02 120ms), got %q", best.Node)
	}
	// 策略二：延迟优先 → 美国Y01（20ms 最快）
	fastest, ok := r.PickBestNodeBy(PickByDelay)
	if !ok {
		t.Fatal("应能挑出节点")
	}
	if fastest.Node != "美国Y01" {
		t.Errorf("延迟优先应选美国Y01(20ms), got %q", fastest.Node)
	}
}

// TestPickBestNodeFallbackAllDead 全部不可用时回落（不返回 ok=false 让调用方无措）。
func TestPickBestNodeFallbackAllDead(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK},
	})
	r.ApplyDelays(map[string]int{"香港Y01": 0})
	if _, ok := r.PickBestNode(); !ok {
		t.Error("全部不可用时仍应返回一个候选（回落），而不是失败")
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
	// 交还自动 → 按地区优先应回到香港（地区优先于延迟）
	l, ok := r.ClearNodeForAccount("u1")
	if !ok {
		t.Fatal("交还自动应成功")
	}
	if l.Node != "香港Y01" {
		t.Errorf("自动分配应选地区优先的香港Y01, got %q", l.Node)
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

// TestPickBestNodeRegionThenFastest 关键回归（实测缺陷）：
// 地区优先 ≠ 排序取第一个；必须在该地区内挑延迟最低的。
//
// 实测背景：香港有 35ms 节点时，原实现选中了同地区 60ms 的节点
// （因为它只取"按端口排序后的第一个健康节点"）。
func TestPickBestNodeRegionThenFastest(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "hk60", Port: 34567, Node: "香港Y01", Region: RegionHK},
		{Name: "hk35", Port: 34568, Node: "香港Y10", Region: RegionHK},
		{Name: "jp20", Port: 34569, Node: "日本Y01", Region: RegionJP},
	})
	// 日本 20ms 最快，但地区优先级低于香港
	r.ApplyDelays(map[string]int{"香港Y01": 60, "香港Y10": 35, "日本Y01": 20})
	best, ok := r.PickBestNode()
	if !ok {
		t.Fatal("应能挑出节点")
	}
	if best.Node != "香港Y10" {
		t.Errorf("应在香港内挑延迟最低的(香港Y10 35ms), got %q", best.Node)
	}
}

// TestPickBestNodeMissingDelays 同地区节点都缺延迟数据时回落（不返回空）。
func TestPickBestNodeMissingDelays(t *testing.T) {
	r := NewRegistry([]Listener{
		{Name: "hk", Port: 34567, Node: "香港Y01", Region: RegionHK},
	})
	r.ApplyDelays(map[string]int{}) // 无延迟数据 → 健康判定为不健康
	if _, ok := r.PickBestNode(); !ok {
		t.Error("全部无延迟数据时仍应返回候选（回落）")
	}
}
