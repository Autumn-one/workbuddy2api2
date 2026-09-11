package main

import "testing"

// ─────────────── 监听地址的展示与复制 ───────────────
//
// 需求：监听地址前加 "http://"，并添加一个快捷复制按钮。
//
// 关键：监听地址允许省略主机名（Go 的 ":7863" = 监听所有网卡）。
// 直接拼 "http://" + ":7863" 会得到 "http://:7863"——这不是能用的 URL，
// 用户粘到浏览器/curl 里会失败。因此需要把空主机补成可用的回环地址。

// TestFormatListenURL 地址格式化：补协议、补空主机、保留原始端口。
func TestFormatListenURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通回环地址", "127.0.0.1:8787", "http://127.0.0.1:8787"},
		{"空主机（监听全网卡）补回环", ":7863", "http://127.0.0.1:7863"},
		{"0.0.0.0 转回环（0.0.0.0 不可直接访问）", "0.0.0.0:8787", "http://127.0.0.1:8787"},
		{"已有 http:// 前缀不重复添加", "http://127.0.0.1:8787", "http://127.0.0.1:8787"},
		{"已有 https:// 前缀保留", "https://example.com:443", "https://example.com:443"},
		{"两侧空白被裁剪", "  127.0.0.1:8787  ", "http://127.0.0.1:8787"},
		{"空串返回空", "", ""},
		{"纯空白返回空", "   ", ""},
		{"IPv6 地址", "[::1]:8787", "http://[::1]:8787"},
		{"只有端口（无冒号前缀）", "8787", "http://127.0.0.1:8787"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatListenURL(c.in); got != c.want {
				t.Errorf("formatListenURL(%q)=%q want %q", c.in, got, c.want)
			}
		})
	}
}

// TestFormatListenURLEdge 边界：不 panic、不产生非法 URL。
func TestFormatListenURLEdge(t *testing.T) {
	for _, in := range []string{":", "0.0.0.0:", ":::", "http://", " ", "\t", "localhost:8787"} {
		got := formatListenURL(in)
		if got == "http://" {
			t.Errorf("输入 %q 产生了无主机的非法 URL: %q", in, got)
		}
		t.Logf("formatListenURL(%q) = %q", in, got)
	}
}

// TestCopyHintLabelIndependent 复制结果必须显示在 lblCopyHint 上，而不是 lblState。
// 原因：lblState 每 1.5s 被 refreshStatus 重写为"运行中 · 已运行 X"，提示会被立刻冲掉。
func TestCopyHintLabelIndependent(t *testing.T) {
	a := &app{}
	a.doCopyListenAddr() // lblAddr 为 nil → 应直接返回，不 panic
	// lblCopyHint 存在时才有反馈；这里只断言调用安全
	_ = a.lblCopyHint
}
