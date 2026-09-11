// listenurl.go — 监听地址的展示与复制。
//
// 服务页把监听地址显示为可点击复制的 URL（带 http:// 前缀）。
// 注意 Go 的监听地址允许省略主机名（":7863" = 监听所有网卡），直接拼 "http://"
// 会得到 "http://:7863" 这种不能用的 URL——用户粘进浏览器/curl 会失败。
// 因此空主机与 0.0.0.0（不可直接访问）都要补成回环地址 127.0.0.1。
package main

import (
	"net"
	"strings"
)

// formatListenURL 把监听地址转成可直接访问/复制的 URL。
//   - 空串 → 空串（未配置时不显示假地址）；
//   - 已有 http:// / https:// 前缀 → 原样返回（不重复添加）；
//   - 空主机（":7863"）/ 0.0.0.0 / :: → 补成 127.0.0.1（本机可访问）；
//   - 只有端口（"8787"）→ 补成 127.0.0.1:8787；
//   - IPv6 字面量（"[::1]:8787"）保持原样。
func formatListenURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	lower := strings.ToLower(addr)
	for _, pfx := range []string{"http://", "https://"} {
		if strings.HasPrefix(lower, pfx) {
			// 只有协议没有主机（"http://"）属残缺输入：补回环，避免输出不可用 URL。
			if strings.TrimSpace(addr[len(pfx):]) == "" {
				return pfx + "127.0.0.1"
			}
			return addr
		}
	}

	host, port := splitHostPort(addr)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		// 空主机与"监听所有网卡"都不是可直接访问的地址 → 回环。
		host = "127.0.0.1"
	}
	// 无端口（含 ":port" 之外的退化形态如 ":"、"0.0.0.0:"）：不能输出
	// "http://host:" —— 那是非法 URL。此时按"只有主机"处理：是端口号就补端口，
	// 否则当主机名（缺失端口由客户端按 http 默认 80 处理）。
	if port == "" {
		if isAllDigits(addr) {
			return "http://127.0.0.1:" + addr
		}
		return "http://" + host
	}
	if strings.HasPrefix(host, "[") {
		return "http://" + host + ":" + port // 已是 IPv6 字面量
	}
	return "http://" + net.JoinHostPort(host, port)
}

// listenURLText 供界面显示：未配置时显示占位符，否则显示带协议的 URL。
func listenURLText(addr string) string {
	u := formatListenURL(addr)
	if u == "" {
		return "—"
	}
	return u
}

// isAllDigits 报告 s 是否为纯十进制数字（用于识别"只给了端口号"的写法）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// splitHostPort 拆分 host:port；无法拆分时返回 (addr, "")。
// 与 net.SplitHostPort 的区别：容忍缺失端口（返回整串作为 host 语义的兜底）。
func splitHostPort(addr string) (host, port string) {
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	// 形如 "0.0.0.0:" 或 ":" 的退化形态
	if strings.HasSuffix(addr, ":") {
		return strings.TrimSuffix(addr, ":"), ""
	}
	if !strings.Contains(addr, ":") {
		return addr, ""
	}
	return addr, ""
}
