// userlisteners.go — 从 Clash 配置里读出【用户自己配置的】listeners。
//
// 为什么需要：
//   - 用户可能已经把 listeners 写进了 Verge 的 Merge 覆写文件（订阅更新也不会丢），
//     或直接粘进了渲染出的配置里；
//   - 那样它们用的端口完全可以不是我们默认的 34567 起；
//   - 只有读出"用户的端口 → 节点"这张表，我们才能真正做到
//     "能用你的就用你的、连端口都不动"，而不是另起一批端口。
//
// 与网关注入块的关系：本程序写入的 listeners 被 autoBlockBegin/End 标记包着，
// 解析时整段排除——它是我们自己的东西，不属于"用户配置"。
package proxy

import (
	"strings"
)

// UserListener 用户自己配置的一个 listener。
type UserListener struct {
	Name string
	Port int
	Node string
}

// ParseUserListeners 从 Clash 配置文本里解析用户配置的 listeners。
//
// 解析策略（容忍 YAML 的常见写法，不做完整 YAML 解析）：
//   - 只处理 listeners: 段（到下一个顶格的非列表行结束）；
//   - 段内每个条目以 "- ..." 开头，条目内识别 port / proxy / name 键；
//   - proxy 键的值即绑定的节点名（去引号）；没有 proxy 的 listener 不是
//     "固定出口"的 listener（例如普通的 http 端口），跳过——网关要的是
//     "一条 listener 固定走一个节点"这种形态。
//   - 自动注入块（autoBlockBegin..End）整段跳过。
//
// 端口非法（<=0 或 >65535）或节点为空的条目一律跳过（宁可不认，也不要误用）。
func ParseUserListeners(configText string) []UserListener {
	lines := strings.Split(configText, "\n")
	out := []UserListener{}
	skipping := false
	inSection := false
	var cur *UserListener
	flush := func() {
		if cur != nil && cur.Port > 0 && cur.Port <= 65535 && cur.Node != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == autoBlockBegin {
			flush()
			skipping = true
			inSection = false
			continue
		}
		if trimmed == autoBlockEnd {
			skipping = false
			continue
		}
		if skipping {
			continue
		}
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		isItem := strings.HasPrefix(trimmed, "- ") || trimmed == "-"

		if !inSection {
			// 段头：顶格的 "listeners:"
			if !indented && trimmed == "listeners:" {
				inSection = true
			}
			continue
		}
		// 段内：顶格且不是列表项，且不是空行 → 说明进入下一个顶层段，段结束。
		// （YAML 的 listeners 列表项可以写在顶格，所以不能把"顶格"一律当段结束。）
		if !indented && trimmed != "" && !isItem {
			flush()
			inSection = false
			continue
		}
		if isItem {
			flush()
			cur = &UserListener{}
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			applyListenerKV(cur, rest)
			continue
		}
		if cur == nil {
			continue
		}
		applyListenerKV(cur, trimmed)
	}
	flush()
	return out
}

// applyListenerKV 处理条目内的一行 "key: value"。
func applyListenerKV(l *UserListener, kv string) {
	if kv == "" {
		return
	}
	key, val, ok := strings.Cut(kv, ":")
	if !ok {
		return
	}
	key = strings.ToLower(strings.TrimSpace(key))
	val = unquoteYAML(strings.TrimSpace(val))
	switch key {
	case "port":
		l.Port = atoiLoose(val)
	case "proxy":
		l.Node = val
	case "name":
		l.Name = val
	}
}

// unquoteYAML 去掉 YAML 字符串的引号（单/双引号）。
func unquoteYAML(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// atoiLoose 宽松解析十进制数字（非法 → 0）。
func atoiLoose(s string) int {
	n := 0
	seen := false
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		seen = true
		n = n*10 + int(r-'0')
		if n > 1<<20 {
			return 0
		}
	}
	if !seen {
		return 0
	}
	return n
}

// UserListenersToListeners 把用户 listeners 转成注册表用的 Listener（按地区排序在 Registry 里做）。
func UserListenersToListeners(us []UserListener) []Listener {
	out := make([]Listener, 0, len(us))
	for _, u := range us {
		name := u.Name
		if name == "" {
			name = "user-" + itoa(u.Port)
		}
		out = append(out, Listener{
			Name:   name,
			Port:   u.Port,
			Node:   u.Node,
			Region: RegionFromNodeName(u.Node),
		})
	}
	return out
}

// CoverAllNodes 报告这些 listener 是否覆盖了给定节点集合（用于如实告知用户
// "哪些节点没有对应端口"）。
func CoverAllNodes(ls []Listener, nodes []string) (missing []string) {
	have := make(map[string]bool, len(ls))
	for _, l := range ls {
		have[l.Node] = true
	}
	for _, n := range nodes {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing
}
