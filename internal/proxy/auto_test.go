package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─────────────── Clash 配置自动化应用 ───────────────
//
// 用户要求：「这个代理的配置应该做得更自动化一点，不要让我粘贴」。
//
// 实测确认的可行路径（2026-09-13）：
//  1. Verge 的 Merge.yaml 不被自动监听（写入后 mihomo 不会重载）；
//  2. 但 PUT /configs {"path": "<完整配置>"} 能在【运行时】重载配置并让
//     listeners 立即生效（实测新增端口 39998/39997 后立即监听成功）。
//
// 因此自动化流程 = 把 listeners 注入到 Verge 渲染出的完整配置（clash-verge.yaml）
// → PUT /configs 重载。无需用户手工粘贴、无需重启 Clash。

// TestInjectListenersIntoConfig 注入 listeners 到已有配置文本。
func TestInjectListenersIntoConfig(t *testing.T) {
	base := "mixed-port: 10808\nmode: global\nlisteners:\n- name: npm-task\n  type: http\n  port: 17898\nrules:\n- MATCH,DIRECT\n"
	ls := []Listener{
		{Name: "acct-01-HK", Port: 34567, Node: "🇭🇰 香港Y01", Region: RegionHK},
		{Name: "acct-02-JP", Port: 34568, Node: "🇯🇵 日本Y01", Region: RegionJP},
	}
	out, err := InjectListeners(base, ls)
	if err != nil {
		t.Fatal(err)
	}
	// 原有 listener 必须保留
	if !strings.Contains(out, "npm-task") || !strings.Contains(out, "17898") {
		t.Errorf("原有 listener 被破坏:\n%s", out)
	}
	// 新 listener 必须存在且字段正确
	for _, want := range []string{"acct-01-HK", "acct-02-JP", "port: 34567", "port: 34568",
		`proxy: "🇭🇰 香港Y01"`, `proxy: "🇯🇵 日本Y01"`} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q:\n%s", want, out)
		}
	}
	// 结构不得损坏（rules 段必须还在）
	if !strings.Contains(out, "rules:") || !strings.Contains(out, "MATCH,DIRECT") {
		t.Errorf("注入破坏了后续配置段:\n%s", out)
	}
}

// TestInjectListenersNoExistingSection 配置里没有 listeners 段时追加。
func TestInjectListenersNoExistingSection(t *testing.T) {
	base := "mixed-port: 10808\nmode: global\nrules:\n- MATCH,DIRECT\n"
	ls := []Listener{{Name: "a", Port: 34567, Node: "香港Y01", Region: RegionHK}}
	out, err := InjectListeners(base, ls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "listeners:") || !strings.Contains(out, "port: 34567") {
		t.Errorf("未追加 listeners 段:\n%s", out)
	}
	if !strings.Contains(out, "rules:") {
		t.Errorf("原有配置丢失:\n%s", out)
	}
}

// TestInjectListenersIdempotent 重复注入不得重复追加（幂等）。
func TestInjectListenersIdempotent(t *testing.T) {
	base := "mixed-port: 10808\n"
	ls := []Listener{{Name: "acct-01-HK", Port: 34567, Node: "香港Y01", Region: RegionHK}}
	once, err := InjectListeners(base, ls)
	if err != nil {
		t.Fatal(err)
	}
	// 对已注入的结果再注入：应替换而非累加
	twice, err := InjectListeners(once, ls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(twice, "acct-01-HK"); n != 1 {
		t.Errorf("重复注入产生 %d 份 listener（应幂等为 1 份）:\n%s", n, twice)
	}
}

// TestInjectListenersRemovesPreviousAutoBlock 旧的自动注入块要被替换（节点变化时）。
func TestInjectListenersRemovesPreviousAutoBlock(t *testing.T) {
	base := "mixed-port: 10808\n"
	first := []Listener{{Name: "acct-01-HK", Port: 34567, Node: "香港Y01", Region: RegionHK}}
	out1, _ := InjectListeners(base, first)
	// 节点变了（端口/名字都变）
	second := []Listener{{Name: "acct-01-JP", Port: 34570, Node: "日本Y01", Region: RegionJP}}
	out2, err := InjectListeners(out1, second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "34567") {
		t.Errorf("旧的自动 listener 未清除:\n%s", out2)
	}
	if !strings.Contains(out2, "34570") {
		t.Errorf("新的自动 listener 未注入:\n%s", out2)
	}
}

// TestInjectListenersEmptyList 空列表：只清理旧的自动块，不报错。
func TestInjectListenersEmptyList(t *testing.T) {
	base := "mixed-port: 10808\n"
	ls := []Listener{{Name: "acct-01-HK", Port: 34567, Node: "香港Y01", Region: RegionHK}}
	out1, _ := InjectListeners(base, ls)
	out2, err := InjectListeners(out1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "34567") {
		t.Errorf("空列表应清理自动注入的 listener:\n%s", out2)
	}
}

// TestFindVergeConfigPath 自动定位 Verge 渲染出的完整配置。
// 用临时目录模拟真实目录结构。
func TestFindVergeConfigPath(t *testing.T) {
	dir := t.TempDir()
	// 模拟 Verge 目录
	full := filepath.Join(dir, "clash-verge.yaml")
	os.WriteFile(full, []byte("mixed-port: 10808\nproxies:\n"), 0o600)
	small := filepath.Join(dir, "config.yaml")
	os.WriteFile(small, []byte("mixed-port: 10808\n"), 0o600)

	got, err := FindRenderedConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != full {
		t.Errorf("应定位到完整配置(clash-verge.yaml), got %s", got)
	}
}

// TestFindRenderedConfigMissing 目录里没有完整配置时报错（调用方据此降级为提示用户）。
func TestFindRenderedConfigMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := FindRenderedConfig(dir); err == nil {
		t.Error("无配置时应报错，而不是返回空路径")
	}
}

// TestAutoBlockMarker 自动注入的块必须有标记（便于幂等替换与用户识别）。
func TestAutoBlockMarker(t *testing.T) {
	if autoBlockBegin == "" || autoBlockEnd == "" {
		t.Fatal("自动块标记不得为空")
	}
	if !strings.Contains(autoBlockBegin, "wb2api") {
		t.Errorf("标记应含 wb2api 便于识别: %q", autoBlockBegin)
	}
}

// TestGenerateReloadPayload 重载 payload 含 path 字段（实测唯一起作用的形式）。
func TestGenerateReloadPayload(t *testing.T) {
	p := ReloadPayload("C:/x/clash-verge.yaml")
	if p["path"] != "C:/x/clash-verge.yaml" {
		t.Errorf("payload 应含 path: %v", p)
	}
}
