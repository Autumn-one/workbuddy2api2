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
