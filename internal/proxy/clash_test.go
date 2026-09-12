package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeClash 起一个假的 Clash API（只提供 GET /proxies）。
func fakeClash(t *testing.T, proxies map[string]string, secret string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret != "" && r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/proxies" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := map[string]any{"proxies": map[string]any{}}
		pm := body["proxies"].(map[string]any)
		for name, typ := range proxies {
			pm[name] = map[string]any{"type": typ}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchNodesFiltersGroups 只返回真实节点，过滤策略组与内置项。
func TestFetchNodesFiltersGroups(t *testing.T) {
	srv := fakeClash(t, map[string]string{
		"🇭🇰 香港Y01": "Shadowsocks",
		"🇯🇵 日本Y01": "Vmess",
		"GLOBAL":   "Selector",
		"NPM-TASK": "Selector",
		"DIRECT":   "Direct",
		"REJECT":   "Reject",
		"AUTO":     "URLTest",
	}, "")
	nodes, err := FetchNodes(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("应只返回 2 个真实节点, got %d: %v", len(nodes), nodes)
	}
	for _, n := range nodes {
		if n == "GLOBAL" || n == "DIRECT" || n == "AUTO" {
			t.Errorf("策略组/内置项不应被当作节点: %q", n)
		}
	}
}

// TestFetchNodesWithSecret 带 secret 时必须发对 Authorization 头。
func TestFetchNodesWithSecret(t *testing.T) {
	srv := fakeClash(t, map[string]string{"香港Y01": "Shadowsocks"}, "autumn")
	if _, err := FetchNodes(srv.URL, "autumn"); err != nil {
		t.Fatalf("正确 secret 应成功: %v", err)
	}
	if _, err := FetchNodes(srv.URL, "wrong"); err == nil {
		t.Error("错误 secret 应失败（否则说明没发鉴权头）")
	}
}

// TestFetchNodesEmpty 无节点时返回错误（调用方据此降级为直连）。
func TestFetchNodesEmpty(t *testing.T) {
	srv := fakeClash(t, map[string]string{"GLOBAL": "Selector"}, "")
	if _, err := FetchNodes(srv.URL, ""); err == nil {
		t.Error("只有策略组时应报错，而不是返回空列表当成功")
	}
}

// TestBuildListenersPriorityOrder 生成顺序必须 HK → TW → JP → 其他。
func TestBuildListenersPriorityOrder(t *testing.T) {
	nodes := []string{
		"🇺🇸 美国Y01", "🇯🇵 日本Y01", "🇭🇰 香港Y01", "🇨🇳 台湾Y01", "🇸🇬 新加坡Y01",
	}
	ls := BuildListeners(nodes, 34567)
	if len(ls) != 5 {
		t.Fatalf("listener 数=%d want 5", len(ls))
	}
	wantOrder := []Region{RegionHK, RegionTW, RegionJP, RegionOther, RegionOther}
	for i, w := range wantOrder {
		if ls[i].Region != w {
			t.Errorf("第 %d 个地区=%v want %v（顺序: %s）", i, ls[i].Region, w, ls[i].Node)
		}
	}
	// 端口连续
	for i, l := range ls {
		if l.Port != 34567+i {
			t.Errorf("第 %d 个端口=%d want %d", i, l.Port, 34567+i)
		}
	}
}

// TestBuildListenersStableOrder 同样输入必须产生同样输出（可复现，避免端口漂移）。
func TestBuildListenersStableOrder(t *testing.T) {
	nodes := []string{"🇯🇵 日本Y01", "🇭🇰 香港Y01", "🇭🇰 香港Y02", "🇺🇸 美国Y01"}
	a := BuildListeners(nodes, 34567)
	// 打乱输入
	b := BuildListeners([]string{"🇺🇸 美国Y01", "🇭🇰 香港Y02", "🇯🇵 日本Y01", "🇭🇰 香港Y01"}, 34567)
	if len(a) != len(b) {
		t.Fatal("长度不一致")
	}
	for i := range a {
		if a[i].Node != b[i].Node || a[i].Port != b[i].Port {
			t.Errorf("第 %d 项不一致: %v/%d vs %v/%d", i, a[i].Node, a[i].Port, b[i].Node, b[i].Port)
		}
	}
}

// TestRenderListenersYAML 生成的 YAML 必须可粘贴：含必要字段、节点名被引号包裹。
func TestRenderListenersYAML(t *testing.T) {
	ls := BuildListeners([]string{"🇭🇰 香港Y01", "🇯🇵 日本Y01"}, 34567)
	out := RenderListenersYAML(ls)

	if !strings.HasPrefix(out, "listeners:\n") {
		t.Errorf("应以 listeners: 开头:\n%s", out)
	}
	for _, want := range []string{
		"type: mixed", "port: 34567", "port: 34568",
		`proxy: "🇭🇰 香港Y01"`, `proxy: "🇯🇵 日本Y01"`, "udp: false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("YAML 缺 %q:\n%s", want, out)
		}
	}
	// 节点名必须被引号包裹（含 emoji/空格/竖线，不加引号 YAML 会解析失败）
	if strings.Contains(out, "proxy: 🇭🇰") {
		t.Error("节点名未被引号包裹，YAML 会解析失败")
	}
}

// TestPortBaseDefault 未指定端口基数时用默认值（非常用端口）。
func TestPortBaseDefault(t *testing.T) {
	ls := BuildListeners([]string{"🇭🇰 香港Y01"}, 0)
	if ls[0].Port != DefaultPortBase {
		t.Errorf("默认端口基数=%d want %d", ls[0].Port, DefaultPortBase)
	}
	if DefaultPortBase < 1024 || DefaultPortBase > 65535 {
		t.Errorf("DefaultPortBase=%d 不在合法范围", DefaultPortBase)
	}
}
