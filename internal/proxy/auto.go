// auto.go — Clash 配置的自动化应用（无需用户手工粘贴）。
//
// 用户要求：代理配置要自动化，不要让人粘贴。
//
// 实测确认的可行路径（2026-09-13）：
//  1. Verge 的 Merge.yaml【不被自动监听】——写入后 mihomo 不会重载（实测：写 Merge.yaml
//     后 10 秒，端口未监听、渲染产物 mtime 未变）；
//  2. 但 PUT /configs {"path": "<完整配置>"} 能在【运行时】重载并让 listeners 立即生效
//     （实测：追加端口 39998/39997 后调用重载，两端口立即监听成功）；
//  3. Verge 渲染出的完整配置是 clash-verge.yaml（含 proxies/proxy-groups/rules/listeners），
//     而 config.yaml 只是 464 字节的骨架（不含 subscribers 内容）。
//
// 因此自动化流程 = 把 listeners 注入 clash-verge.yaml → PUT /configs 重载。
// 全程无需用户操作，也无需重启 Clash。
//
// 安全性：所有写入都是"插入一个带标记的块"，原有配置一字不改；
// 出错时任一步失败都返回错误，调用方降级为提示用户（不留下半成品配置）。
package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// autoBlockBegin/End 自动注入块的标记：便于幂等替换，也让用户一眼看出这段是工具写的。
	autoBlockBegin = "# >>> wb2api auto listeners >>>"
	autoBlockEnd   = "# <<< wb2api auto listeners <<<"
)

// FindRenderedConfig 在 Verge 数据目录里定位"渲染出的完整配置"。
//
// 判定依据：clash-verge.yaml 体积远大于 config.yaml（后者是骨架），
// 且含 proxies 段。找不到时返回错误（调用方降级为提示用户）。
func FindRenderedConfig(dataDir string) (string, error) {
	cands := []string{
		filepath.Join(dataDir, "clash-verge.yaml"),
		filepath.Join(dataDir, "clash-verge-check.yaml"),
	}
	for _, p := range cands {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// 必须是完整配置：含 proxies 段（骨架 config.yaml 不含）
		if strings.Contains(string(raw), "proxies:") {
			return p, nil
		}
	}
	return "", errors.New("未找到 Verge 渲染的完整配置（clash-verge.yaml）")
}

// InjectListeners 把 listeners 注入配置文本，返回新文本。
//
// 行为：
//   - 先移除上一次注入的自动块（幂等；节点变化时替换为新块）；
//   - 插入到已有 `listeners:` 段内（保留用户原有 listener，如 npm-task）；
//   - 没有 listeners 段时追加一个；
//   - ls 为空表示只清理自动块。
func InjectListeners(configText string, ls []Listener) (string, error) {
	// 1) 移除旧的自动块
	cleaned := removeAutoBlock(configText)

	if len(ls) == 0 {
		return cleaned, nil
	}

	// 2) 构造新的自动块
	var b strings.Builder
	b.WriteString(autoBlockBegin + "\n")
	for _, l := range ls {
		// 与原配置里的 listener 格式保持一致（listen 显式写 127.0.0.1）
		fmt.Fprintf(&b, "- name: %s\n", l.Name)
		b.WriteString("  type: mixed\n")
		b.WriteString("  listen: 127.0.0.1\n")
		fmt.Fprintf(&b, "  port: %d\n", l.Port)
		fmt.Fprintf(&b, "  proxy: %q\n", l.Node)
		b.WriteString("  udp: false\n")
	}
	b.WriteString(autoBlockEnd + "\n")
	block := b.String()

	// 3) 插入到 listeners: 段内
	lines := strings.Split(cleaned, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "listeners:" {
			continue
		}
		// 找到该段的结束（下一个顶格的非列表行）
		insertAt := i + 1
		for insertAt < len(lines) {
			l := lines[insertAt]
			// 段内行：顶格 "- " 开头，或缩进行
			if l == "" || strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "-") ||
				strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
				insertAt++
				continue
			}
			break
		}
		out := append([]string{}, lines[:insertAt]...)
		out = append(out, strings.Split(strings.TrimRight(block, "\n"), "\n")...)
		out = append(out, lines[insertAt:]...)
		return strings.Join(out, "\n"), nil
	}

	// 4) 没有 listeners 段：追加到末尾（保留末尾换行）。
	// 必须补上段头 "listeners:" —— 只写条目的话 YAML 会把那些键挂到上一个段上，
	// 导致配置语义错误（实测漏写时 entries 被解析成了上层的键）。
	out := strings.TrimRight(cleaned, "\n") + "\nlisteners:\n" + block
	return out, nil
}

// removeAutoBlock 移除上一次注入的自动块（含标记行之间的内容）。
func removeAutoBlock(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, l := range lines {
		switch {
		case strings.TrimSpace(l) == autoBlockBegin:
			skipping = true
			continue
		case strings.TrimSpace(l) == autoBlockEnd:
			skipping = false
			continue
		case skipping:
			continue
		default:
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// ReloadPayload 构造 PUT /configs 的 payload。
// 实测唯一起作用的形式是 {"path": "<配置文件路径>"}（带 listeners 字段会被忽略）。
func ReloadPayload(configPath string) map[string]any {
	return map[string]any{"path": configPath}
}

// ApplyListeners 一站式自动化：注入 listeners → 写回配置 → 重载生效。
//
// 返回写入的配置路径。任一步失败都返回错误（不留下半成品：写回前先备份到内存，
// 失败时恢复原内容）。
func ApplyListeners(apiBase, secret, dataDir string, ls []Listener) (string, error) {
	cfgPath, err := FindRenderedConfig(dataDir)
	if err != nil {
		return "", err
	}
	orig, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", fmt.Errorf("读取配置失败: %w", err)
	}
	injected, err := InjectListeners(string(orig), ls)
	if err != nil {
		return "", fmt.Errorf("注入 listeners 失败: %w", err)
	}
	if injected == string(orig) {
		return cfgPath, nil // 无变化，无需重载
	}
	// 原子写回（tmp + rename）：失败不破坏原配置
	tmp := cfgPath + ".wb2api.tmp"
	if err := os.WriteFile(tmp, []byte(injected), 0o600); err != nil {
		return "", fmt.Errorf("写入临时配置失败: %w", err)
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("替换配置失败: %w", err)
	}
	// 重载；失败则回滚配置内容（避免磁盘与运行态不一致）
	code, body := RawConfigsPut(apiBase, secret, ReloadPayload(cfgPath))
	if code != 200 && code != 204 {
		_ = os.WriteFile(cfgPath, orig, 0o600)
		return "", fmt.Errorf("Clash 重载失败（%d %s），已回滚配置", code, body)
	}
	return cfgPath, nil
}
