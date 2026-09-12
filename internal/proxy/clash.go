// clash.go — 与本地 Clash/mihomo 交互：读取节点列表、生成 listeners 配置。
//
// 为什么需要生成 listeners：Clash 的节点切换是【全局状态】，并发请求会互相踩。
// 用 listeners 给每个节点开一个专属本地端口（34567 → 香港Y01），网关按端口固定出口，
// 与并发天然兼容。
//
// 只读 Clash：仅调用 GET /proxies 读节点列表（不改任何配置）。生成的 listeners 配置
// 由用户粘到 Clash 的 Merge 覆写文件（Verge 的 profiles/Merge.yaml，订阅更新不覆盖）。
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultClashAPI 本地 Clash 外部控制地址（Verge 默认）。
const DefaultClashAPI = "http://127.0.0.1:9097"

// DefaultPortBase listeners 的起始端口（用户要求：非常用端口）。
// 34567 起连续分配，避开 Windows 保留区间与常见服务端口。
const DefaultPortBase = 34567

// clashProxiesResp GET /proxies 的响应。
type clashProxiesResp struct {
	Proxies map[string]struct {
		Type string `json:"type"`
	} `json:"proxies"`
}

// nonNodeTypes 不是真实节点的类型（策略组/内置）。
var nonNodeTypes = map[string]bool{
	"Selector": true, "URLTest": true, "Fallback": true, "LoadBalance": true,
	"Direct": true, "Reject": true, "Compatible": true, "Pass": true,
	"RejectDrop": true, "Dns": true, "Relay": true,
}

// FetchNodes 从 Clash API 读取真实节点名（不含策略组/内置）。
// 失败返回错误，由调用方决定是否降级（例如无 Clash 时保持直连）。
func FetchNodes(apiBase, secret string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiBase, "/")+"/proxies", nil)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	cli := &http.Client{Timeout: 8 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clash api status %d", resp.StatusCode)
	}
	var data clashProxiesResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	var out []string
	for name, p := range data.Proxies {
		if nonNodeTypes[p.Type] {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("clash api 未返回任何节点")
	}
	sort.Strings(out)
	return out, nil
}

// BuildListeners 按地区优先级（HK → TW → JP → 其他）为每个节点生成一个 listener。
// 端口从 portBase 起连续分配。
func BuildListeners(nodes []string, portBase int) []Listener {
	if portBase <= 0 {
		portBase = DefaultPortBase
	}
	sorted := make([]string, len(nodes))
	copy(sorted, nodes)
	// 先地区、再原名（稳定可复现）
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := RegionFromNodeName(sorted[i]), RegionFromNodeName(sorted[j])
		if ri != rj {
			return ri < rj
		}
		return sorted[i] < sorted[j]
	})
	out := make([]Listener, 0, len(sorted))
	for i, n := range sorted {
		r := RegionFromNodeName(n)
		out = append(out, Listener{
			// listener 名保持 ASCII（HK/TW/JP/OT）：
			// Clash 的 name 会出现在日志与 API 里，非 ASCII 可能带来编码/兼容问题，
			// 而地区信息对网关无实际用途（真正的地区在 Node 名里，界面显示那个）。
			Name:   fmt.Sprintf("acct-%02d-%s", i+1, asciiRegion(r)),
			Port:   portBase + i,
			Node:   n,
			Region: r,
		})
	}
	return out
}

// asciiRegion listener 名用的 ASCII 地区码。
func asciiRegion(r Region) string {
	switch r {
	case RegionHK:
		return "HK"
	case RegionTW:
		return "TW"
	case RegionJP:
		return "JP"
	default:
		return "OT"
	}
}

// RenderListenersYAML 生成可粘贴到 Clash Merge 覆写文件的 listeners 配置。
// 节点名用双引号包裹（含 emoji/空格/竖线，必须引起来）。
func RenderListenersYAML(ls []Listener) string {
	var b strings.Builder
	b.WriteString("listeners:\n")
	for _, l := range ls {
		fmt.Fprintf(&b, "  - name: %s\n", l.Name)
		b.WriteString("    type: mixed\n")
		fmt.Fprintf(&b, "    port: %d\n", l.Port)
		fmt.Fprintf(&b, "    proxy: %q\n", l.Node)
		b.WriteString("    udp: false\n")
	}
	return b.String()
}

// rawGET 向 Clash API 发 GET 并返回 (状态码, 响应体片段)。仅用于诊断/验证。
func rawGET(apiBase, secret, path string) (int, string) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiBase, "/")+path, nil)
	if err != nil {
		return 0, err.Error()
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// RawConfigsPut 向 Clash 的 PUT /configs 发送原始 payload（用于探测与重载）。
// 返回 (状态码, 响应体片段)。仅用于诊断与自动化重载。
func RawConfigsPut(apiBase, secret string, payload map[string]any) (int, string) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err.Error()
	}
	req, err := http.NewRequest(http.MethodPut, strings.TrimRight(apiBase, "/")+"/configs", bytes.NewReader(raw))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	cli := &http.Client{Timeout: 10 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// ConfigFilePath 读取 Clash 当前使用的配置文件路径（GET /configs 的 path 字段）。
func ConfigFilePath(apiBase, secret string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(apiBase, "/")+"/configs", nil)
	if err != nil {
		return "", err
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	cli := &http.Client{Timeout: 8 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var d struct {
		Path string `json:"path"`
	}
	if json.NewDecoder(resp.Body).Decode(&d) != nil {
		return "", errDecode
	}
	return d.Path, nil
}

var errDecode = errors.New("clash configs decode failed")
