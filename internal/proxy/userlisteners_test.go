package proxy

import (
	"strings"
	"testing"
)

// ─────────────── 读取"用户自己配置的 listeners" ───────────────
//
// 用户要求（2026-09-13）：不要每次启动都去改他的 Clash 配置。
// 前提是能识别"他已经配好了"——包括端口不是我们默认的那批。

// TestParseUserListenersBasic 解析用户 listeners：端口 → 节点。
func TestParseUserListenersBasic(t *testing.T) {
	cfg := `mixed-port: 10808
listeners:
- name: npm-task
  type: http
  listen: 127.0.0.1
  port: 17898
- name: my-hk
  type: mixed
  listen: 127.0.0.1
  port: 40001
  proxy: "🇭🇰 香港Y01"
  udp: false
- name: my-jp
  type: mixed
  listen: 127.0.0.1
  port: 40002
  proxy: "🇯🇵 日本Y01"
rules:
- MATCH,DIRECT
`
	got := ParseUserListeners(cfg)
	if len(got) != 2 {
		t.Fatalf("应解析出 2 个（无 proxy 的 npm-task 不算固定出口）, got %d: %+v", len(got), got)
	}
	if got[0].Port != 40001 || got[0].Node != "🇭🇰 香港Y01" {
		t.Errorf("第一条不对: %+v", got[0])
	}
	if got[1].Port != 40002 || got[1].Node != "🇯🇵 日本Y01" {
		t.Errorf("第二条不对: %+v", got[1])
	}
}

// TestParseUserListenersExcludesAutoBlock 本程序自己注入的块不算"用户配置"
// （否则会把自己的产物当成用户意图，从而永远不再更新节点）。
func TestParseUserListenersExcludesAutoBlock(t *testing.T) {
	cfg := `listeners:
- name: my-hk
  port: 40001
  proxy: "香港Y01"
# >>> wb2api auto listeners >>>
- name: acct-01-HK
  type: mixed
  listen: 127.0.0.1
  port: 34567
  proxy: "🇭🇰 香港Y01"
# <<< wb2api auto listeners <<<
rules:
- MATCH,DIRECT
`
	got := ParseUserListeners(cfg)
	if len(got) != 1 || got[0].Port != 40001 {
		t.Fatalf("自动注入块应被排除, got %+v", got)
	}
}

// TestParseUserListenersSectionEnds 跨段不串（listeners 段之后的内容不解析）。
func TestParseUserListenersSectionEnds(t *testing.T) {
	cfg := `listeners:
- name: a
  port: 40001
  proxy: "香港Y01"
proxies:
- name: b
  port: 40002
  proxy: "日本Y01"
`
	got := ParseUserListeners(cfg)
	if len(got) != 1 || got[0].Port != 40001 {
		t.Errorf("只应解析 listeners 段, got %+v", got)
	}
}

// TestParseUserListenersSkipsInvalid 非法端口/缺节点 → 跳过（宁可不认，不误用）。
func TestParseUserListenersSkipsInvalid(t *testing.T) {
	cfg := `listeners:
- name: no-port
  proxy: "香港Y01"
- name: bad-port
  port: abc
  proxy: "香港Y02"
- name: too-big
  port: 99999
  proxy: "香港Y03"
- name: no-node
  port: 40003
- name: ok
  port: 40004
  proxy: "香港Y04"
`
	got := ParseUserListeners(cfg)
	if len(got) != 1 || got[0].Port != 40004 {
		t.Fatalf("只有最后一条合法, got %+v", got)
	}
}

// TestParseUserListenersInlineForm 支持 "listeners: []" 这种空写法与
// 同一条目内联写法（不崩、不产生垃圾条目）。
func TestParseUserListenersInlineForm(t *testing.T) {
	if got := ParseUserListeners("listeners: []\nrules:\n- MATCH,DIRECT\n"); len(got) != 0 {
		t.Errorf("空 listeners 应返回 0 条, got %+v", got)
	}
	if got := ParseUserListeners(""); len(got) != 0 {
		t.Errorf("空文档应返回 0 条, got %+v", got)
	}
}

// TestCoverAllNodes 如实报告"哪些节点没有对应端口"。
func TestCoverAllNodes(t *testing.T) {
	ls := []Listener{{Port: 40001, Node: "香港Y01"}, {Port: 40002, Node: "日本Y01"}}
	missing := CoverAllNodes(ls, []string{"香港Y01", "日本Y01", "香港Y09"})
	if len(missing) != 1 || missing[0] != "香港Y09" {
		t.Errorf("应报告缺失的节点, got %v", missing)
	}
}

// TestParseUserListenersCommentLines 段内注释行不是段边界。
//
// 回归点（实测 2026-10-01，代理开启失败的根因之一）：本机 Clash 配置的 listeners
// 段里带着兄弟项目注入的标记注释 `# >>> trae2api auto listeners >>>`，
// 旧实现把这一行当成"下一个顶层段"→ 段解析提前结束 → 其后 33 条可用端口
// 一条都没被识别 → 误判"用户没配 listeners" → 去改写 Clash（在服务模式下必被拒），
// 开启代理因此失败。
func TestParseUserListenersCommentLines(t *testing.T) {
	cfg := strings.Join([]string{
		"listeners:",
		"- name: npm-task",
		"  type: http",
		"  listen: 127.0.0.1",
		"  port: 17898",
		"# >>> trae2api auto listeners >>>",
		"- name: acct-01-HK",
		"  type: mixed",
		"  listen: 127.0.0.1",
		"  port: 34567",
		`  proxy: "🇭🇰 香港Y01"`,
		"  udp: false",
		"- name: acct-02-HK",
		"  type: mixed",
		"  listen: 127.0.0.1",
		"  port: 34568",
		`  proxy: "🇭🇰 香港Y02 | IEPL"`,
		"  udp: false",
		"# 用户随手写的注释",
		"- name: acct-03-HK",
		"  port: 34569",
		`  proxy: "🇭🇰 香港Y03"`,
		"rules:",
		"- MATCH,DIRECT",
	}, "\n")
	got := ParseUserListeners(cfg)
	want := []struct {
		port int
		node string
	}{
		{34567, "🇭🇰 香港Y01"},
		{34568, "🇭🇰 香港Y02 | IEPL"},
		{34569, "🇭🇰 香港Y03"},
	}
	if len(got) != len(want) {
		t.Fatalf("应解析出 %d 条（注释行不得终止段）, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i].Port != w.port || got[i].Node != w.node {
			t.Errorf("第 %d 条 = %d/%q, want %d/%q", i, got[i].Port, got[i].Node, w.port, w.node)
		}
	}
}

// TestParseUserListenersRealConfigShape 用本机真实配置的形状验证
// （含混合缩进、CRLF、emoji 节点名、竖线与空格）。
func TestParseUserListenersRealConfigShape(t *testing.T) {
	cfg := strings.Join([]string{
		"listeners:",
		"- name: npm-task",
		"  type: http",
		"  listen: 127.0.0.1",
		"  port: 17898",
		"# >>> wb2api auto listeners >>>",
		"- name: acct-01-HK",
		"  type: mixed",
		"  listen: 127.0.0.1",
		"  port: 34567",
		`  proxy: "🇭🇰 香港Y01"`,
		"  udp: false",
		"# <<< wb2api auto listeners <<<",
		"",
	}, "\r\n")
	if got := ParseUserListeners(cfg); len(got) != 0 {
		t.Errorf("只有自动块时用户 listeners 应为空, got %+v", got)
	}
}
