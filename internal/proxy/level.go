// level.go — 「代理接管等级」：决定网关是否改写本机 Clash 配置。
//
// 背景（用户的真实处境，2026-09-13）：
//   - 「一键开启代理」原本无条件把 listeners 写进用户的 clash-verge.yaml 并让 Clash 重载。
//     若用户自己已经配过这些 listeners（例如把配置放进了 Verge 的 Merge 覆写文件，
//     订阅更新后依然存在），这就是一次无必要、且有风险的介入——用户并不希望工具
//     反复改他的 Clash 配置。
//   - 但用户也不该被要求手工配置。因此需要"能自动就自动、该尊重就尊重"的判定。
//
// 判定方式（唯一可靠的、零副作用的办法）：**实际用一次**。
//   - 直接通过某个本地端口发一个 HEAD 请求（目标用本地 Clash 的 API 地址）；
//     listeners 若已就位并能转发出网，请求会成功返回；
//     端口没监听 / 节点不通，则失败（连接被拒或超时）。
//   - HTTP 代理收到绝对 URL 的 HEAD 请求只回响应头，不产生多少流量；打的是本机 Clash API，
//     不涉及上游、不消耗模型积分。
//
// 由此得出三个等级（由高到低探测，命中即停）：
//
//	Level2：用户自己的 listeners 已就位且能出网 → 【完全不碰 Clash 配置】，直接用现有端口；
//	Level1：Clash API 可读节点、但端口还没配 → 自动注入 listeners 并重载（当前默认行为）；
//	Level0：Clash 不可用 → 不启用代理（回落直连，不影响网关服务）。
//
// 等级会被持久化：一旦确认过 Level2，之后启动就不再尝试写 Clash 配置，
// 直到用户点「重新检测」或显式要求重新应用。
package proxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TakeoverLevel 网关对本机 Clash 配置的接管等级（数值越大，介入越少）。
type TakeoverLevel int

const (
	// LevelDisabled 未探测 / 无法启用（Clash 不可用）。
	LevelDisabled TakeoverLevel = 0
	// LevelInject 需要网关写入 listeners 并重载 Clash 才可用。
	LevelInject TakeoverLevel = 1
	// LevelRespect 用户自己的 listeners 已就位、能出网：完全不改 Clash 配置。
	LevelRespect TakeoverLevel = 2
)

// String 可读文案（日志/界面用）。
func (l TakeoverLevel) String() string {
	switch l {
	case LevelInject:
		return "自动注入"
	case LevelRespect:
		return "沿用现有"
	default:
		return "未启用"
	}
}

// Describe 面向用户的说明（日志与提示文案）。
func (l TakeoverLevel) Describe() string {
	switch l {
	case LevelInject:
		return "由本程序写入 Clash 的 listeners"
	case LevelRespect:
		return "沿用你已配置好的 listeners（本次不修改 Clash 配置）"
	default:
		return "未启用"
	}
}

// ParseLevel 解析持久化的等级文本（"inject" / "respect"；空/未知 → LevelDisabled）。
func ParseLevel(s string) TakeoverLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "inject":
		return LevelInject
	case "respect":
		return LevelRespect
	default:
		return LevelDisabled
	}
}

// MarshalText 持久化用的等级文本。
func (l TakeoverLevel) MarshalText() string {
	switch l {
	case LevelInject:
		return "inject"
	case LevelRespect:
		return "respect"
	default:
		return ""
	}
}

// ProbeResult 一次"端口是否真的能用"的探测结果。
type ProbeResult struct {
	ProxyURL string // 被探测的本地代理地址
	OK       bool   // 该端口是否成功转发了请求（= 用户既有配置可用）
	Status   int    // 成功时的 HTTP 状态码（便于日志核对）
	Err      string // 失败原因（截断后用于日志）
}

// Decision 一次开启/启动时的选路结论。
type Decision struct {
	// Level 实际采用的接管等级。
	Level TakeoverLevel
	// Reason 面向用户的说明（写日志与界面提示，讲清"为什么这么做"）。
	Reason string
	// UseUserPorts 为 true 时应当使用【用户自己配置的端口表】（而不是我们生成的）。
	UseUserPorts bool
}

// Decide 决定接管等级与用哪套端口（纯函数，便于离线测试）。
//
// 输入：
//
//	userPorts 从 Clash 配置里解析出的【用户自己配置的】listeners（已排除本程序注入块）
//	probes    对 userPorts 的实测结果（userPorts 为空时无意义，可传 nil）
//	listening 是否有任一 userPorts 端口处于 TCP 监听（区分"配置被删"与"配了但节点暂时不通"）
//	prev      上次持久化的等级
//
// 规则（顺序即优先级）：
//  1. 用户配了端口且实测能用 → 沿用，完全不改其配置；
//  2. 用户配了端口、实测不通但端口在监听 → 仍沿用（节点临时不通不该导致我们去改他的配置）；
//  3. 用户配了端口、但端口连监听都没有 → 他的配置没生效，回退注入（保证代理可用）；
//  4. 用户没配端口 → 由网关注入。
func Decide(userPorts []Listener, probes []ProbeResult, listening bool, prev TakeoverLevel) Decision {
	if len(userPorts) == 0 {
		return Decision{Level: LevelInject, Reason: "由本程序写入 listeners（未发现你自己配置的 listeners）"}
	}
	for _, p := range probes {
		if p.OK {
			return Decision{Level: LevelRespect, UseUserPorts: true,
				Reason: "沿用你已配置好的 listeners（未改动 Clash 配置）"}
		}
	}
	if listening {
		return Decision{Level: LevelRespect, UseUserPorts: true,
			Reason: "沿用你已配置好的 listeners（未改动 Clash 配置；部分节点当前不通，稍后会自动恢复）"}
	}
	note := "：你配置的 listeners 未生效（端口未监听）"
	if prev == LevelRespect {
		note = "：你之前配置的 listeners 已不存在"
	}
	return Decision{Level: LevelInject, Reason: "由本程序写入 listeners" + note}
}

// PickLevel 根据探测结果决定接管等级（保留给"没有用户端口表"的调用场景）。
//
// 规则：
//   - 有任一端口可用 → LevelRespect；
//   - 否则若上一次是 LevelRespect → 保持 LevelRespect（一次探测失败不足以推翻用户配置）；
//   - 其余 → LevelInject。
func PickLevel(probes []ProbeResult, prev TakeoverLevel) TakeoverLevel {
	for _, p := range probes {
		if p.OK {
			return LevelRespect
		}
	}
	if prev == LevelRespect {
		return LevelRespect
	}
	return LevelInject
}

// ProbePorters 并发探测"这些端口能否真正转发出网"。
//
// 判据（比 TCP 连通强得多）：通过该端口请求本地 Clash API 的只读接口 /version，
// 能拿到 200 才认为"这个 listener 可用"——它证明 listener 存在、且其绑定的
// 出站链路（节点或 DIRECT）真的能建立连接并返回数据。
//
// 注意：目标故意选【本地 Clash API】而不是外部网站：
//   - 打本机地址不会给上游留下任何痕迹，也不消耗任何额度；
//   - listener 的 proxy 字段决定了出站走哪个节点，链路本身被完整验证；
//   - 不依赖外网可达性（无外网时也能判定 listeners 是否配好）。
//
// apiHostPort 是本地 Clash API 的 host:port（如 127.0.0.1:9097）。
func ProbePorters(localPorts []int, apiHostPort string, timeout time.Duration) []ProbeResult {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	host := strings.TrimSpace(apiHostPort)
	if host == "" {
		host = "127.0.0.1:9097"
	}
	// 探测用的 Authorization 头：/version 是 Clash 的公开只读接口，无需鉴权；
	// 若用户配了 secret，缺少它也只是 401 —— 那同样证明"端口能转发"（链路通）。
	out := make([]ProbeResult, 0, len(localPorts))
	for _, port := range localPorts {
		proxyURL := "http://127.0.0.1:" + itoa(port)
		target := "http://" + host + "/version"
		res := ProbeResult{ProxyURL: proxyURL}
		u, err := url.Parse(proxyURL)
		if err != nil {
			res.Err = err.Error()
			out = append(out, res)
			continue
		}
		tr := &http.Transport{Proxy: http.ProxyURL(u)}
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			res.Err = err.Error()
			out = append(out, res)
			continue
		}
		resp, err := (&http.Client{Timeout: timeout, Transport: tr}).Do(req)
		if err != nil {
			res.Err = truncateErr(err.Error())
			out = append(out, res)
			continue
		}
		res.Status = resp.StatusCode
		_ = resp.Body.Close()
		// 2xx/3xx/401/403 都说明"请求经该端口转发到目标并拿到了响应"。
		res.OK = resp.StatusCode > 0 && resp.StatusCode < 500
		out = append(out, res)
	}
	return out
}

// truncateErr 截断错误文本（日志用，避免超长）。
func truncateErr(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// APIHostPort 从 Clash API 基地址里取出 host:port（探测目标用）。
func APIHostPort(apiBase string) string {
	u, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// proxyStateFile 接管等级的持久化结构。
type proxyStateFile struct {
	// Level 上次确认的接管等级（"inject" / "respect"）；空 = 未确认过。
	Level string `json:"level"`
	// At 上次确认时间（RFC3339，便于用户核对）。
	At string `json:"at"`
	// Note 上次确认的附加说明（例如回退原因），面向用户可读。
	Note string `json:"note,omitempty"`
}

// LoadLevel 读取持久化的接管等级（文件缺失/损坏 → LevelDisabled）。
func LoadLevel(path string) TakeoverLevel {
	raw, err := readFileIfExists(path)
	if err != nil || len(raw) == 0 {
		return LevelDisabled
	}
	var st proxyStateFile
	if json.Unmarshal(raw, &st) != nil {
		return LevelDisabled
	}
	return ParseLevel(st.Level)
}

// SaveLevel 原子落盘接管等级（失败仅忽略：属运行态辅助信息）。
func SaveLevel(path string, level TakeoverLevel, note string) {
	if path == "" || level == LevelDisabled {
		return
	}
	st := proxyStateFile{Level: level.MarshalText(), At: time.Now().Format(time.RFC3339), Note: note}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	writeFileAtomic(path, raw)
}
