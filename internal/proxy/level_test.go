package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// ─────────────── 代理接管等级 ───────────────
//
// 用户要求（2026-09-13）：
//  1. 启动时自动开启代理（不用每次手点）；
//  2. 默认沿用上次记录的账号→节点对应关系；
//  3. 只有发现对应关系失效（本机代理/节点变了）才重新分配。
//
// 补一条由现实推导出的要求：如果用户自己已经把 listeners 配好了（例如写进
// Verge 的 Merge 覆写文件），工具就不该再去改他的 Clash 配置——那就成了
// 无必要、且会覆盖用户意图的介入。

// TestPickLevelRespectWhenProbeOK 用户既有配置可用 → 沿用，绝不改配置。
func TestPickLevelRespectWhenProbeOK(t *testing.T) {
	probes := []ProbeResult{{ProxyURL: "http://127.0.0.1:34567", OK: true, Status: 200}}
	if got := PickLevel(probes, LevelDisabled); got != LevelRespect {
		t.Errorf("端口可用时应为 LevelRespect, got %v", got.Describe())
	}
}

// TestPickLevelInjectWhenNothingWorks 端口都不通、也没确认过沿用 → 由网关注入。
func TestPickLevelInjectWhenNothingWorks(t *testing.T) {
	probes := []ProbeResult{{ProxyURL: "http://127.0.0.1:34567", Err: "connection refused"}}
	if got := PickLevel(probes, LevelDisabled); got != LevelInject {
		t.Errorf("端口不可用时应回落到 LevelInject, got %v", got.Describe())
	}
	// 没探测过（空）同理
	if got := PickLevel(nil, LevelDisabled); got != LevelInject {
		t.Errorf("未探测时应回落到 LevelInject, got %v", got.Describe())
	}
}

// TestPickLevelKeepsRespectOnTransientFailure 上次确认过"沿用"，
// 这次探测失败（节点临时不通等）不得擅自改回注入——那是改用户配置。
func TestPickLevelKeepsRespectOnTransientFailure(t *testing.T) {
	probes := []ProbeResult{{ProxyURL: "http://127.0.0.1:34567", Err: "timeout"}}
	if got := PickLevel(probes, LevelRespect); got != LevelRespect {
		t.Errorf("上次为 respect 时应保持 respect, got %v", got.Describe())
	}
}

// TestDecideUserPortsUsable 用户配了端口且实测能用 → 沿用，绝不改配置。
func TestDecideUserPortsUsable(t *testing.T) {
	user := []Listener{{Name: "my-hk", Port: 40001, Node: "香港Y01"}, {Name: "my-jp", Port: 40002, Node: "日本Y01"}}
	probes := []ProbeResult{{ProxyURL: "http://127.0.0.1:40001", OK: true, Status: 200}}

	d := Decide(user, probes, true, LevelInject)
	if d.Level != LevelRespect || !d.UseUserPorts {
		t.Fatalf("应沿用用户端口, got %+v", d)
	}
}

// TestDecideUserPortsPresentButNodeDown 用户配了端口、节点暂时不通 →
// 仍沿用（一次不通不该成为改写他配置的理由）。
func TestDecideUserPortsPresentButNodeDown(t *testing.T) {
	user := []Listener{{Name: "my-hk", Port: 40001, Node: "香港Y01"}}
	probes := []ProbeResult{{ProxyURL: "http://127.0.0.1:40001", Err: "timeout"}}

	d := Decide(user, probes, true /* 端口在监听 */, LevelRespect)
	if d.Level != LevelRespect || !d.UseUserPorts {
		t.Fatalf("端口在监听但节点不通时应沿用, got %+v", d)
	}
}

// TestDecideUserPortsGone 用户配的端口连监听都没有 → 回退注入（否则代理就废了）。
func TestDecideUserPortsGone(t *testing.T) {
	user := []Listener{{Name: "my-hk", Port: 40001, Node: "香港Y01"}}
	probes := []ProbeResult{{ProxyURL: "http://127.0.0.1:40001", Err: "connection refused"}}

	d := Decide(user, probes, false, LevelRespect)
	if d.Level != LevelInject || d.UseUserPorts {
		t.Fatalf("端口未监听时应回退注入, got %+v", d)
	}
	if d.Reason == "" {
		t.Error("回退时要有面向用户的原因说明")
	}
}

// TestDecideNoUserPorts 用户没配 listeners → 由网关注入。
func TestDecideNoUserPorts(t *testing.T) {
	d := Decide(nil, nil, false, LevelDisabled)
	if d.Level != LevelInject || d.UseUserPorts {
		t.Fatalf("没有用户配置时应注入, got %+v", d)
	}
}

// TestDecideIgnoresGatewayOwnPorts 本程序自己注入的 listeners 不算"用户配置"
// （由 ParseUserListeners 排除）；这里锁定：即使端口能用，也只认 userPorts 表。
func TestDecideIgnoresGatewayOwnPorts(t *testing.T) {
	// 探测结果里有可用端口，但 userPorts 为空（说明那个可用端口是我们自己配的）
	probes := []ProbeResult{{ProxyURL: "http://127.0.0.1:34567", OK: true, Status: 200}}
	d := Decide(nil, probes, true, LevelInject)
	if d.Level != LevelInject {
		t.Fatalf("没有用户配置时应走注入（不能被自己的端口误导）, got %+v", d)
	}
}

// TestTakeoverLevelPersistence 等级要能落盘并在重启后读回（否则每次启动
// 都会重新写用户的 Clash 配置）。
func TestTakeoverLevelPersistence(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "proxy-state.json")
	if got := LoadLevel(fp); got != LevelDisabled {
		t.Errorf("文件不存在时应为 LevelDisabled, got %v", got.Describe())
	}
	SaveLevel(fp, LevelRespect, "沿用既有 listeners")
	if got := LoadLevel(fp); got != LevelRespect {
		t.Errorf("应读回 LevelRespect, got %d", got)
	}
	SaveLevel(fp, LevelInject, "")
	if got := LoadLevel(fp); got != LevelInject {
		t.Errorf("应读回 LevelInject, got %d", got)
	}
	// 空路径 / 非法等级不写不崩
	SaveLevel("", LevelRespect, "")
	SaveLevel(fp, LevelDisabled, "不应覆盖")
	if got := LoadLevel(fp); got != LevelInject {
		t.Errorf("LevelDisabled 不应写入覆盖, got %d", got)
	}
}

// TestParseLevel 持久化文本解析（含容错）。
func TestParseLevel(t *testing.T) {
	cases := map[string]TakeoverLevel{
		"inject": LevelInject, "respect": LevelRespect, " RESPECT ": LevelRespect,
		"": LevelDisabled, "unknown": LevelDisabled,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q)=%v want %v", in, got, want)
		}
	}
}

// TestProbePortersRealForwarding 端口探测必须真的"用一次"：
// 起一个假代理（它就是目标），验证"经该端口能拿到响应"被判定为 OK，
// 而端口没监听时判定为不可用。
func TestProbePortersRealForwarding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":"fake"}`))
	}))
	defer srv.Close()
	target := srv.Listener.Addr().String() // 127.0.0.1:<port>

	// 找一个空闲端口充当"代理端口"：直接用该 server 自己 —— 它是 HTTP 服务器
	// 而不是代理，所以经它发绝对 URL 请求也能拿到 200（足够证明链路通）。
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	got := ProbePorters([]int{port}, target, 2*time.Second)
	if len(got) != 1 || !got[0].OK {
		t.Fatalf("可用端口应判定 OK, got %+v", got)
	}
	if got[0].Status != http.StatusOK {
		t.Errorf("状态码应被记录, got %d", got[0].Status)
	}

	// 未监听的端口 → 不可用（1 号端口在本机不可能有人监听）
	dead := ProbePorters([]int{1}, target, 800*time.Millisecond)
	if len(dead) != 1 || dead[0].OK {
		t.Errorf("未监听端口应判定不可用, got %+v", dead)
	}
	if dead[0].Err == "" {
		t.Error("失败时应记录原因（便于日志核对）")
	}
}

// TestAPIHostPort 从 API 基地址取 host:port（探测目标）。
func TestAPIHostPort(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:9097": "127.0.0.1:9097",
		"127.0.0.1:9097":        "",
		"":                      "",
	}
	for in, want := range cases {
		if got := APIHostPort(in); got != want {
			t.Errorf("APIHostPort(%q)=%q want %q", in, got, want)
		}
	}
}
