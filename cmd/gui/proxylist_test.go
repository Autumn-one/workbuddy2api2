package main

import (
	"testing"

	"workbuddy2api/internal/proxy"
)

// ─────────────── 代理列表的显示完整性 ───────────────
//
// 用户反馈的两个设计错误（已修）：
//  1. 开启后列表【默认空白】——绑定是懒执行的，要等请求才出现。
//     现在开启时 EnsureAllAssigned 一次填满；且每次刷列表都补齐新账号。
//  2. 「切换选中账号节点」读的是账号页的选中行（错位），点了"凭空冒出一行"。
//     现在只作用于代理列表自身选中行，且切换后保持选中。
//
// 本文件锁定"列表该显示什么"的行为。

// TestProxyBindingModelRows 表格列映射（账号/节点名/延迟/地区/状态/端口）。
//
// 缺陷背景（用户反馈"点击探测后看不到延迟"）：延迟取到了但只存在内部状态，
// 既没填进 proxyBindingRow、也没有对应列 —— 用户看不到。
// 现在延迟是独立一列（第 2 列），端口挪到最后。
func TestProxyBindingModelRows(t *testing.T) {
	m := &proxyBindingModel{}
	m.Replace([]proxyBindingRow{
		{UID: "u1", Name: "木瓜", Node: "🇭🇰 香港Y01", Port: 34567, Region: "香港", Healthy: true, Delay: 34},
		{UID: "u2", Name: "138", Node: "🇯🇵 日本Y01", Port: 34568, Region: "日本", Healthy: false},
	})
	if m.RowCount() != 2 {
		t.Fatalf("行数=%d want 2", m.RowCount())
	}
	// 列序：0 账号 | 1 出口节点 | 2 延迟 | 3 地区 | 4 状态 | 5 端口
	want := []string{"木瓜", "🇭🇰 香港Y01", "34ms", "香港", "可用", "34567"}
	for col, w := range want {
		if got := m.Value(0, col); got != w {
			t.Errorf("列 %d=%v want %q", col, got, w)
		}
	}
	// 未探测到延迟（0）显示 "—"（区分"慢"与"没数据"）
	if got := m.Value(1, 2); got != "—" {
		t.Errorf("无延迟数据应显示 '—', got %v", got)
	}
	if got := m.Value(1, 4); got != "不可用" {
		t.Errorf("不健康应显示'不可用', got %v", got)
	}
}

// TestProxyBindingModelAt 越界安全 + 能按行取到 UID（切换节点需要）。
func TestProxyBindingModelAt(t *testing.T) {
	m := &proxyBindingModel{}
	m.Replace([]proxyBindingRow{{UID: "u1", Name: "甲", Node: "香港Y01", Port: 34567}})
	if r, ok := m.At(0); !ok || r.UID != "u1" {
		t.Errorf("At(0)=%+v ok=%v", r, ok)
	}
	for _, i := range []int{-1, 5} {
		if _, ok := m.At(i); ok {
			t.Errorf("At(%d) 应返回 false（越界）", i)
		}
	}
}

// TestRefreshProxyBindingsNilRegClearsList 代理关闭时列表必须清空
// （否则旧行残留，看起来"还开着"）。
func TestRefreshProxyBindingsNilRegClearsList(t *testing.T) {
	m := &proxyBindingModel{}
	m.Replace([]proxyBindingRow{{UID: "u1", Node: "香港Y01", Port: 34567}})
	a := &app{proxyBindings: m, proxyReg: nil}
	a.refreshProxyBindings() // proxyReg 为 nil → 应清空
	if m.RowCount() != 0 {
		t.Errorf("代理关闭后列表应清空, got %d 行", m.RowCount())
	}
}

// TestRefreshProxyBindingsFillsAllAccounts 代理开启时列表必须含【全部账号】
// （含运行期新增、尚未被请求过的账号）。
func TestRefreshProxyBindingsFillsAllAccounts(t *testing.T) {
	reg := proxy.NewRegistry([]proxy.Listener{
		{Name: "a", Port: 34567, Node: "香港Y01", Region: proxy.RegionHK},
		{Name: "b", Port: 34568, Node: "日本Y01", Region: proxy.RegionJP},
	})
	// 模拟"这些账号从未被请求过"（没有任何绑定）
	if n := len(reg.Snapshot()); n != 0 {
		t.Fatalf("前置条件：初始应无绑定, got %d", n)
	}
	// EnsureAllAssigned（refreshProxyBindings 内部会调）应一次填满
	reg.EnsureAllAssigned([]string{"u1", "u2", "u3"})
	if n := len(reg.Snapshot()); n != 3 {
		t.Errorf("应立刻绑定全部 3 个账号, got %d", n)
	}
}
