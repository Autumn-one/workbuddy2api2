package main

import "testing"

// ─────────────── 原地重启的目标 exe 解析 ───────────────
//
// 「改名替换」部署：运行中的 wb2api-gui.exe 被改名成 wb2api-gui.prevN.exe、
// 新版占了原名。此时 os.Executable() 返回的是 .prevN 路径，重启若直接拉它
// 会起回旧版——必须按后缀约定找回原名上的新 exe。

// TestSwappedOutExeName 命中改名约定时还原原名。
func TestSwappedOutExeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"wb2api-gui.prev.exe", "wb2api-gui.exe"},
		{"wb2api-gui.prev2.exe", "wb2api-gui.exe"},
		{"wb2api-gui.prev12.exe", "wb2api-gui.exe"},
	}
	for _, c := range cases {
		got, ok := swappedOutExeName(c.in)
		if !ok || got != c.want {
			t.Errorf("swappedOutExeName(%q) = (%q,%v), want (%q,true)", c.in, got, ok, c.want)
		}
	}
}

// TestSwappedOutExeNameMiss 非备份命名不误判（否则会拉错 exe）。
func TestSwappedOutExeNameMiss(t *testing.T) {
	for _, in := range []string{
		"wb2api-gui.exe",       // 正常名：没有 .prev
		"wb2api-gui.prev",      // 不是 .exe 结尾
		".prev.exe",            // 原名为空
		"wb2api-gui.prevx.exe", // .prev 与 .exe 之间有非数字
		"preview.exe",          // 含 "prev" 但不是 .prevN 约定
		"wb2api.prev.exe.bak",  // .exe 不在结尾
	} {
		if got, ok := swappedOutExeName(in); ok {
			t.Errorf("swappedOutExeName(%q) 不应命中, got (%q,true)", in, got)
		}
	}
}
