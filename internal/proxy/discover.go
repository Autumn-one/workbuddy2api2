// discover.go — 自动发现本地 Clash 的 API 端点（用户无需手工填写）。
//
// 用户反馈：「代理功能怎么用？我怎么还得在 config.json 里设置参数、重启才行？
// 这也太难用了，搞简单一点行吗？」
//
// 实测：Clash 的 external-controller 与 secret 就写在它自己的配置里，
// 完全可以自动读出来——用户不该手工填这些。
//
// 读取顺序：Verge 目录下的完整配置（clash-verge.yaml）→ 骨架（config.yaml）→ 默认值。
package proxy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ClashEndpoint 本地 Clash 的 API 连接信息。
type ClashEndpoint struct {
	API    string // 如 http://127.0.0.1:9097
	Secret string // 可为空（Clash 未设密码时）
}

// parseEndpointRe 逐键提取（只取首个匹配）。
var (
	reExternalController = regexp.MustCompile(`(?m)^\s*external-controller\s*:\s*(\S.*?)\s*$`)
	reSecret             = regexp.MustCompile(`(?m)^\s*secret\s*:\s*(\S.*?)\s*$`)
)

// ParseClashEndpoint 从 Clash 配置文本解析 API 端点。
// fallbackAPI 非空时优先使用（尊重用户显式配置）；否则用配置里的值；都没有则默认 9097。
func ParseClashEndpoint(configText, fallbackAPI string) ClashEndpoint {
	ep := ClashEndpoint{API: strings.TrimSpace(fallbackAPI)}
	if ep.API == "" {
		if m := reExternalController.FindStringSubmatch(configText); m != nil {
			host := strings.Trim(strings.TrimSpace(m[1]), `"'`)
			if host != "" {
				// 缺协议前缀时补 http://（Clash 配置里通常只写 host:port）
				if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
					host = "http://" + host
				}
				ep.API = host
			}
		}
	}
	if ep.API == "" {
		ep.API = "http://127.0.0.1:9097"
	}
	if m := reSecret.FindStringSubmatch(configText); m != nil {
		ep.Secret = strings.Trim(strings.TrimSpace(m[1]), `"'`)
	}
	return ep
}

// DiscoverClashEndpoint 从 Verge 数据目录自动发现端点。
// 目录里没有配置时返回默认值（不报错——本机可能未装 Clash，或用了别的客户端）。
func DiscoverClashEndpoint(dataDir, fallbackAPI string) (ClashEndpoint, error) {
	if dataDir == "" {
		return ParseClashEndpoint("", fallbackAPI), nil
	}
	// 优先完整配置（含真实 secret），其次骨架
	for _, name := range []string{"clash-verge.yaml", "config.yaml"} {
		raw, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			continue
		}
		ep := ParseClashEndpoint(string(raw), fallbackAPI)
		// 骨架不含 proxies，但含 external-controller/secret，足够用
		return ep, nil
	}
	return ParseClashEndpoint("", fallbackAPI), nil
}

// VerifyEndpoint 验证端点可用（调 /version，Clash 的公开只读接口）。
// 返回 (是否可用, 错误信息)。用于 GUI 开关时给出明确反馈。
func VerifyEndpoint(api, secret string) (bool, string) {
	code, body := rawGET(api, secret, "/version")
	if code == 0 {
		return false, body
	}
	if code == 401 || code == 403 {
		return false, "Clash API 鉴权失败（secret 不匹配）"
	}
	if code != 200 {
		return false, "Clash API 返回 " + itoa(code)
	}
	if !strings.Contains(body, "version") && body == "" {
		return false, "Clash API 响应异常"
	}
	return true, ""
}
