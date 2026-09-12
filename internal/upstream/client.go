// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone        ErrKind = iota // 成功
	ErrHardCredit                 // 余额不足（402 或 body 关键词）→ 长冷却
	ErrSoftRate                   // 429 软限流 → 短冷却
	ErrSessionDead                // 401 + 12153 offline session 失效 → 禁用
	ErrNotFound                   // 404 上游偶发 → 短冷却，不累计错误计数（防雪崩）
	ErrServer                     // 5xx 上游故障
	ErrClient                     // 其他 4xx / 业务错误
	// ErrModelRateLimit 模型级频率限制（观测专用，当前【不触发任何冷却】）。
	//
	// 实测文案（WorkBuddy 客户端，2026-09-11）：
	//   "当前您在Deepseek-V4.1-Flash模型的使用量已超出频率限制，
	//    可在2026-09-11 22:56:11 重置可用。您可切换其他模型或消耗积分继续使用该模型"
	//
	// 已实证的结论：
	//   1) 是【模型级】而非账号级——文案限定到具体模型，且提示"可切换其他模型"；
	//   2) 文案里的"重置可用"时刻【不可信】——实测预报 22:56:11，但 02:52 已可正常调用，
	//      且连续 5 次调用积分零消耗（排除"降级扣积分"假设），故该时刻仅为保守上界；
	//   3) 不冻结账号——账号积分仍充足，其他模型照常可用。
	//
	// 因此当前仅做【识别 + 详细记录】，不做冷却处置：先观测真实频率与分布，
	// 拿到足够样本后再决定退避策略。切勿据报文时间冷却（会误伤数十小时）。
	ErrModelRateLimit

	// ErrContentRejected 内容安全拒绝（code=11140，HTTP 403）。
	//
	// 实测文案（2026-09-12）：
	//   {"code":11140,"msg":"request illegal",
	//    "displayMsg":{"zh":"内容未通过安全审核，请调整后重试",
	//                  "en":"The content did not pass the safety review..."}}
	//
	// 生产实证（关键，决定了处置策略）：
	//   账号 eedf4e88：11140 共 189 次、成功 0 次（100% 被拒）
	//   账号 d4937369：11140 共 4 次、成功 0 次
	//   其他 10 个账号：11140 共 0 次、成功 1000+ 次
	//   且每次都「该账号撞 11140 → 换号 → 立刻 200」——【同一个请求体换账号就通过】。
	//
	// 因此这【不是内容问题，而是账号被上游风控标记】：若真是内容违规，换账号
	// 也应被拒。按"客户端错误只换号不罚"处理会让被标记的账号反复被选中、
	// 每次白撞一次上游往返（并让请求多一跳延迟），故需要独立的短期冷却。
	ErrContentRejected
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrModelRateLimit:
		return "model_rate_limit"
	case ErrContentRejected:
		return "content_rejected"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// modelRateLimitMarkers 模型级频率限制关键词。
//
// 判定策略：必须【同时】命中"频率限制类"与"模型/切换"语境才算，避免把
// 泛化的 429 限流（应走 ErrSoftRate）误吸进来。实测文案同时含：
//   - 频率限制类："超出频率限制" / "frequency limit" / "rate limit"
//   - 模型语境："模型的使用量" / "切换其他模型" / "模型" + "重置可用"
var modelRateLimitMarkers = []string{
	"超出频率限制", "频率限制", "模型的使用量",
	"超出使用频率", "频率已达上限",
	"frequency limit", "model usage limit", "rate limit exceeded",
}

// modelContextMarkers 模型语境关键词（与频率限制类关键词合取判定）。
var modelContextMarkers = []string{
	"切换其他模型", "其他模型", "模型的使用量", "该模型", "模型已",
	"another model", "other model", "switch model",
}

// ModelRateLimitEvidence 模型级频率限制（code 6004）的观测证据。
// Model 取自报文 msg 字段里上游点名的模型（如 "deepseek-v4.1-flash 的使用量已超出…"），
// 仅用于日志展示与交叉核对；真正的冷却键仍以【本请求实际使用的模型】为准
// （st.model），因为发往上游的就是它——报文里没提或提取失败时以请求模型兜底。
type ModelRateLimitEvidence struct {
	Status int    // HTTP 状态码
	Model  string // 报文里点名的模型（可能为空：上游没点名就不猜）
	Msg    string // 原始报文片段（截断）
}

// isModelRateLimit 判定报文是否描述"模型级频率限制"。
// 合取条件：(命中频率限制类词) AND (命中模型语境词 OR 同时出现"模型/model"与"重置/reset")。
func isModelRateLimit(body string) bool {
	lb := strings.ToLower(body)
	freq := false
	for _, m := range modelRateLimitMarkers {
		if strings.Contains(body, m) || strings.Contains(lb, strings.ToLower(m)) {
			freq = true
			break
		}
	}
	if !freq {
		return false
	}
	for _, m := range modelContextMarkers {
		if strings.Contains(body, m) || strings.Contains(lb, strings.ToLower(m)) {
			return true
		}
	}
	// 兜底：含"模型"/"model"字样 且 含"重置"/"reset"（实测文案特征）也算。
	hasModelWord := strings.Contains(body, "模型") || strings.Contains(lb, "model")
	hasResetWord := strings.Contains(body, "重置") || strings.Contains(lb, "reset")
	return hasModelWord && hasResetWord
}

// uidPrefixForLog 日志用的账号标识：昵称前 7 字符优先，无昵称回落 UID 前 8 位，空值 "-"。
// 与 server 包请求表格日志的 acct= 口径一致，方便按账号对照两处日志。
// 参数用 *auth.Auth 是因为改写日志的调用链上拿到的就是它（昵称直接可达）。
func uidPrefixForLog(a *auth.Auth) string {
	if a == nil {
		return "-"
	}
	if name := strings.TrimSpace(a.Nickname); name != "" {
		rs := []rune(name)
		if len(rs) > 7 {
			return string(rs[:7])
		}
		return name
	}
	if a.UID == "" {
		return "-"
	}
	if len(a.UID) > 8 {
		return a.UID[:8]
	}
	return a.UID
}

// ParseModelRateLimitFromMsg 从报文文本提取模型级频率限制证据。
// 返回 ok=false 表示不是该类限制。仅用于记录，不解析"重置时间"
// （实测该时间不可信，故刻意不提取，避免下游误用）。
func ParseModelRateLimitFromMsg(status int, msg string) (ModelRateLimitEvidence, bool) {
	if !isModelRateLimit(msg) {
		return ModelRateLimitEvidence{}, false
	}
	return ModelRateLimitEvidence{
		Status: status,
		Model:  parseModelRateLimitModel(msg),
		Msg:    truncate(msg, 300),
	}, true
}

// parseModelRateLimitModel 从 6004 报文里提取上游点名的模型名。
// 实测文案形如："您对模型的使用量已超出频率限制" / "模型 [xxx] 的使用量已超出频率限制"。
// 提取策略（保守）：优先取「模型 [X]」/「模型 X 的」中的 X；取不到返回空串，绝不猜测。
func parseModelRateLimitModel(msg string) string {
	// 形式一：模型 [X]
	if i := strings.Index(msg, "模型 ["); i >= 0 {
		rest := msg[i+len("模型 ["):]
		if j := strings.Index(rest, "]"); j > 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	// 形式二：模型 X 的使用量
	if i := strings.Index(msg, "模型 "); i >= 0 {
		rest := msg[i+len("模型 "):]
		if j := strings.Index(rest, " 的使用量"); j > 0 {
			return strings.TrimSpace(rest[:j])
		}
		if j := strings.Index(rest, "的使用量"); j > 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	return ""
}

// contentRejectMarkers 内容安全拒绝的判据：业务码 11140（权威）+ 文案兜底。
// 只认 code 字段能避免把其它 4xx 误吸进来；文案作为 code 缺失时的补充。
var contentRejectMarkers = []string{
	"did not pass the safety review", "safety review",
}

// isContentRejection 判定报文是否为内容安全拒绝（code=11140）。
// 只解析 code 字段：字符串型 code（"11140"）保守不认，避免类型混乱导致误判；
// 截断/畸形 JSON 返回 false（绝不 panic）。
func isContentRejection(body string) bool {
	if body == "" {
		return false
	}
	var env struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && env.Code != nil {
		if *env.Code == 11140 {
			return true
		}
		// code 存在且不是 11140：不做文案兜底，避免误判（文案可能出现在别的错误里）。
		return false
	}
	// code 解析不到（非 JSON / 截断）→ 文案兜底（仅英文特征串，语义明确）。
	lower := strings.ToLower(body)
	for _, m := range contentRejectMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序（重要）：
//  1. 402 → ErrHardCredit（最确定的余额信号，优先）
//  2. 硬余额关键词 → ErrHardCredit
//  3. 模型级频率限制 → ErrModelRateLimit【必须早于硬余额关键词之外的一切】
//  4. session dead / 429 / 404 / 5xx / 4xx
//
// 第 3 步位置说明：模型级频率限制文案不含余额关键词（已实测），因此不会被第 2 步截走；
// 但它可能伴随 200/400/429 各种状态码。放在 429/4xx 之前，保证不被 ErrSoftRate/ErrClient
// 抢先归类——否则该现象将永远无法被观测到。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	// 模型级频率限制：先于 429/404/4xx 判定，确保可观测。
	if isModelRateLimit(body) {
		return ErrModelRateLimit
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	// 内容安全拒绝（11140）：先于通用 4xx，避免被 ErrClient 吞掉而失去观测与处置。
	// 放在余额/session/6004 之后：那些类别优先级更高（同报文可能叠加多种特征）。
	if isContentRejection(body) {
		return ErrContentRejected
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	effortsMu sync.RWMutex
	efforts   map[string][]string

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	SanitizeFingerprints bool

	// proxySel 账号级代理选择器（nil = 全部直连）。见 proxy.go。
	// 用 atomic.Pointer 而非裸字段：GUI 可在运行期切换（用户点开关立刻生效），
	// 而请求可能正在其他 goroutine 里取值 —— 必须并发安全。
	proxySel atomicProxySel
	// proxyPool 按代理地址缓存 Transport（连接池隔离）。
	proxyPool *proxyPool

	ChatBaseCN    string
	BillingBaseCN string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// 聊天 SSE 首字节前硬上限（对短 RPC 无实际影响：其总时长 120s 更先到期）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		proxyPool:            newProxyPool(120 * time.Second),
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
	}
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

func (c *Client) chatBase(a *auth.Auth) string {
	return c.ChatBaseCN
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
func (c *Client) prepareBody(body []byte, a *auth.Auth) []byte {
	out, _ := c.prepareBodyWithParams(body, a)
	return out
}

// prepareBodyWithParams 组装出站请求体，并同时返回【改写后】的关键参数快照。
// 两者在同一次解析中产生，保证日志展示的参数与真正发出的报文完全一致。
func (c *Client) prepareBodyWithParams(body []byte, a *auth.Auth) ([]byte, EffectiveParams) {
	return PrepareBodyOptWithEffortsAndParams(body, c.SanitizeFingerprints, c.effortsSnapshot(), a)
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot() map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	if len(c.efforts) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(c.efforts))
	for k, v := range c.efforts {
		cp[k] = v
	}
	return cp
}

func (c *Client) billingBase(a *auth.Auth) string {
	return c.BillingBaseCN
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	return c.doJSONFor(nil, req)
}

// doJSONFor 与 doJSON 相同，但按账号选择出口代理（a 为 nil 表示直连）。
func (c *Client) doJSONFor(a *auth.Auth, req *http.Request) (json.RawMessage, error) {
	resp, err := c.clientFor(a, false).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, a)
	data, err := c.doJSONFor(a, req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	rc, status, respBody, _, err = c.ChatStreamWithParams(a, body)
	return rc, status, respBody, err
}

// ChatStreamWithParams 与 ChatStream 行为完全一致，额外返回实际发往上游的关键参数
// （思考档位 / 输出上限）供请求日志展示。
//
// 参数来自与出站报文同一次改写（prepareBodyWithParams），因此日志不会与实际上游请求
// 不一致；对非 JSON 请求体返回零值（日志显示"-"），不影响转发本身。
func (c *Client) ChatStreamWithParams(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, params EffectiveParams, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	prepared, params := c.prepareBodyWithParams(body, a)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, params, err
	}
	ChatHeaders(req, a)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.clientFor(a, true).Do(req)
	if err != nil {
		cancel()
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, params, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, params, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, params, nil
}

// ModelInfo 动态模型信息。
//
// 字段对应上游 /console/enterprises/personal/models 的 data.models[]。
// 上游实测提供 23 个字段，此处透出对使用者有意义的部分（调度/展示/客户端感知）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
	// CreditsText 上游 data.models[].credits 的原文（形如 "x0.51 credits"）。
	// 语义是【消耗倍率】而非额度：同一个账号积分池按倍率折算各模型可用量。
	// 上游对部分模型（auto/hunyuan-chat 等）返回空串或非数值串，此时保持原文不解析。
	CreditsText string
	// CreditsRate 从 CreditsText 解析出的倍率（如 0.51）；无法解析时为 0。
	// 仅作换算展示用，不参与任何调度/计费决策。
	CreditsRate float64

	// ── 思考深度（reasoning）──
	// DefaultEffort 上游 reasoning.defaultEffort：模型默认思考档（如 "high"）。
	// 注意：与 Efforts 是【互斥】的两种表达——上游对固定单档模型只给 defaultEffort，
	// 对可调档模型只给 supportedEfforts。二者都空表示上游未声明。
	DefaultEffort string
	// CanDisableThinking 上游 reasoning.canDisableThinking：是否允许关闭思考。
	CanDisableThinking bool
	// ReasoningSummary 上游 reasoning.summary（实测恒为 "auto"）。
	ReasoningSummary string

	// ── 能力标志 ──
	SupportsImages    bool // 支持图片输入（多模态）
	SupportsReasoning bool // 是推理模型
	SupportsToolCall  bool // 支持工具调用
	OnlyReasoning     bool // 只能推理（无法关闭思考链）

	// ── 描述与元信息 ──
	DescriptionZh string   // 中文简介
	DescriptionEn string   // 英文简介
	Vendor        string   // 厂商标识（e/f/j 等内部代号）
	Tags          []string // 标签（如 "craft"）
	IsDefault     bool     // 是否为默认模型

	// ── 采样默认值 ──
	// 上游直接给出各模型的推荐采样参数；指针语义用零值区分"未提供"。
	Temperature *float64
	TopP        *float64
	TopK        *int64
}

// dynModel 上游 data.models[] 的原始结构（具名类型，便于扩展与避免匿名结构体位置字面量错位）。
type dynModel struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	MaxInputTokens    int64    `json:"maxInputTokens"`
	MaxOutputTokens   int64    `json:"maxOutputTokens"`
	MaxAllowedSize    int64    `json:"maxAllowedSize"`
	Disabled          bool     `json:"disabled"`
	Credits           string   `json:"credits"`
	DescriptionZh     string   `json:"descriptionZh"`
	DescriptionEn     string   `json:"descriptionEn"`
	Vendor            string   `json:"vendor"`
	SupportsImages    bool     `json:"supportsImages"`
	SupportsReasoning bool     `json:"supportsReasoning"`
	SupportsToolCall  bool     `json:"supportsToolCall"`
	OnlyReasoning     bool     `json:"onlyReasoning"`
	IsDefault         bool     `json:"isDefault"`
	Tags              []string `json:"tags"`
	Temperature       *float64 `json:"temperature"`
	TopP              *float64 `json:"top_p"`
	TopK              *int64   `json:"top_k"`
	Reasoning         struct {
		Effort             string   `json:"effort"`
		DefaultEffort      string   `json:"defaultEffort"`
		SupportedEfforts   []string `json:"supportedEfforts"`
		CanDisableThinking bool     `json:"canDisableThinking"`
		Summary            string   `json:"summary"`
	} `json:"reasoning"`
}

// verifiedEffortsSupplement 实测确认、但上游未声明的可选思考档。
//
// 背景：上游对部分【实际可调档】的模型不返回 reasoning.supportedEfforts（只给
// defaultEffort），而网关对未声明的模型不做降级（原样透传）——请求因此仍然生效，
// 但 GUI 与 /v1/models 会把它显示成"固定单档"，与真实能力不符。
//
// 收录条件：必须是【本项目实测确认】过的模型，且必须在上游确实未声明时才有意义。
// 新增条目必须附上实测证据，不得凭推测填入。
var verifiedEffortsSupplement = map[string][]string{
	// 证据：同一难题 4 轮独立实测，max 在全部 4 轮中均为最高（low/high 差距明显）；
	// 与 OpenRouter 公开模型列表独立声明 supported_efforts=["max","high","low"] 一致。
	// 反面证据（已并入文档）：简单题上三档会重叠甚至倒序（曾有 low 59 > high 46 > max 33），
	// 那是信噪比不足，不代表档位失效——收录结论以难题实测为准。
	"deepseek-v4.1-flash": {"low", "high", "max"},
}

// verifiedEfforts 上游未声明 supportedEfforts 时，用实测档位补全【展示用】能力。
//
// 边界：上游一旦声明就以上游为准（本表只填补空缺，不覆盖）。
// 重要：本函数只影响展示（ModelInfo.Efforts → GUI / /v1/models），
// 【不影响】请求体降级缓存——缓存只认上游原始声明，否则 "none"/"off" 这类
// "关闭思考"的请求会被档位下限抬成 "low"，反而改变了既有行为。
func verifiedEfforts(id string, declared []string) []string {
	if len(declared) > 0 {
		return declared
	}
	sup, ok := verifiedEffortsSupplement[id]
	if !ok {
		return declared
	}
	return append([]string(nil), sup...)
}

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.clientFor(a, false).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModel `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]dynModel, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	// rawEfforts 只记录【上游原始声明】的可选档，供请求体降级使用（理由见函数尾注释）。
	rawEfforts := make(map[string][]string, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		if len(m.Reasoning.SupportedEfforts) > 0 {
			rawEfforts[id] = m.Reasoning.SupportedEfforts
		}
		rate, _ := ParseCreditsRate(m.Credits)
		// 上游对固定单档模型只给 reasoning.effort，对可调档模型只给
		// reasoning.supportedEfforts（实测互斥）；默认档优先取 defaultEffort，
		// 回落 effort，保证两类模型都有"默认档"可显示。
		defEffort := m.Reasoning.DefaultEffort
		if defEffort == "" {
			defEffort = m.Reasoning.Effort
		}
		out = append(out, ModelInfo{
			ID:                 m.ID,
			Name:               m.Name,
			ContextWindow:      m.MaxInputTokens,
			MaxTokens:          m.MaxOutputTokens,
			Efforts:            verifiedEfforts(id, m.Reasoning.SupportedEfforts),
			CreditsText:        m.Credits,
			CreditsRate:        rate,
			DefaultEffort:      defEffort,
			CanDisableThinking: m.Reasoning.CanDisableThinking,
			ReasoningSummary:   m.Reasoning.Summary,
			SupportsImages:     m.SupportsImages,
			SupportsReasoning:  m.SupportsReasoning,
			SupportsToolCall:   m.SupportsToolCall,
			OnlyReasoning:      m.OnlyReasoning,
			DescriptionZh:      m.DescriptionZh,
			DescriptionEn:      m.DescriptionEn,
			Vendor:             m.Vendor,
			Tags:               m.Tags,
			IsDefault:          m.IsDefault,
			Temperature:        m.Temperature,
			TopP:               m.TopP,
			TopK:               m.TopK,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级）。
	// 只收录【上游原始声明】的 supportedEfforts：展示层补全的实测档位不参与降级，
	// 否则"关闭思考"类请求（none/off）会被下限抬成 low，改变既有行为。
	c.effortsMu.Lock()
	c.efforts = rawEfforts
	c.effortsMu.Unlock()
	return out, nil
}

// ParseCreditsRate 从上游 credits 原文解析消耗倍率。
// 实测上游取值形如 "x0.51 credits" / "x0.03" / "x2.20 credits"，也可能为空串
// （auto / hunyuan-chat 等模型不返回倍率）。解析失败返回 0,false——调用方据此
// 显示原文而不做换算，绝不猜测。
func ParseCreditsRate(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	t = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "x"), "X"))
	// 取前导数字部分（小数点/正负号），遇到 " credits" 之类后缀即停。
	end := 0
	for end < len(t) {
		c := t[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(t[:end], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// PackageDetail 单个套餐包的额度明细（get-user-resource 的 Accounts[] 一行）。
// 上游积分按【套餐包】发放，同一个账号可能有多行（体验版 + 赠送包等），
// 但全部折算为同一单位 credits 且共享同一积分池——不存在模型维度的额度。
type PackageDetail struct {
	PackageName    string // 如 "CodeBuddy个人体验版"
	ProductName    string // 如 "腾讯云代码助手"
	SubProductName string // 如 "腾讯云代码助手 (IDE) - 赠送包"
	PackageCode    string
	Remain         int64  // 该包剩余积分
	Size           int64  // 该包周期总量
	Used           int64  // 该包已用
	CycleEndTime   string // 周期结束（到期日）
	Unit           string // 单位，实测 "credits"
}

// ResourceDetail 账号额度详情：聚合剩余 + 套餐包明细。
type ResourceDetail struct {
	Remain      int64           // 所有包聚合剩余（与 UserResource 同口径）
	Size        int64           // 所有包聚合总量
	Used        int64           // 所有包聚合已用
	TotalDosage int64           // 上游总配额（作 size 下限）
	Packages    []PackageDetail // 每个套餐包一行
}

// UserResourceDetail 查询账号额度详情（聚合值 + 套餐包明细）。
// 与 UserResource 的聚合口径完全一致（Cycle 优先、负值钳 0），额外透出分包明细。
// UserResource 保留为它的薄封装，既有调用方行为不变。
func (c *Client) UserResourceDetail(a *auth.Auth) (*ResourceDetail, error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSONFor(a, req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64 `json:"TotalDosage"`
				Accounts    []struct {
					PackageName         string `json:"PackageName"`
					ProductName         string `json:"ProductName"`
					SubProductName      string `json:"SubProductName"`
					PackageCode         string `json:"PackageCode"`
					CapacityUnit        string `json:"CapacityUnit"`
					CycleEndTime        string `json:"CycleEndTime"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}

	rd := &ResourceDetail{TotalDosage: resp.Response.Data.TotalDosage}
	for _, acct := range resp.Response.Data.Accounts {
		// 与 UserResource 逐字相同的选取口径：Cycle 优先，否则回落 Capacity；负值钳 0。
		var r, size, used int64
		switch {
		case acct.CycleCapacitySize > 0:
			r, size, used = acct.CycleCapacityRemain, acct.CycleCapacitySize, acct.CycleCapacityUsed
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r, size, used = acct.CycleCapacityRemain, acct.CycleCapacitySize, acct.CycleCapacityUsed
		default:
			r, size, used = acct.CapacityRemain, acct.CapacitySize, acct.CapacityUsed
		}
		if r < 0 {
			r = 0
		}
		rd.Remain += r
		rd.Size += size
		rd.Used += used
		rd.Packages = append(rd.Packages, PackageDetail{
			PackageName:    acct.PackageName,
			ProductName:    acct.ProductName,
			SubProductName: acct.SubProductName,
			PackageCode:    acct.PackageCode,
			Remain:         r,
			Size:           size,
			Used:           used,
			CycleEndTime:   acct.CycleEndTime,
			Unit:           acct.CapacityUnit,
		})
	}
	// TotalDosage 作为 size 下限（与 cmd/credit 口径一致）。
	if rd.TotalDosage > rd.Size {
		rd.Size = rd.TotalDosage
	}
	return rd, nil
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
// 保留原签名与聚合结果；明细见 UserResourceDetail。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	rd, err := c.UserResourceDetail(a)
	if err != nil {
		return 0, err
	}
	return rd.Remain, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	url := c.billingBase(a) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSONFor(a, req)
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// IsAlreadyCheckedIn 判断签到返回的错误是否表示"今天已签到"（属于正常业务提示，不算故障）。
// 已签到走业务 code 非 0 而非 HTTP 错误，措辞可能是中文或英文（实测 code=10001 "今天已签到"）。
func IsAlreadyCheckedIn(msg string) bool {
	if strings.TrimSpace(msg) == "" {
		return false
	}
	s := strings.ToLower(msg)
	// 业务文案（上游重复签到的真实文案：中文「今天已签到，请明天再来」/ 英文 already checked in）。
	for _, m := range alreadyCheckedMarkers {
		if strings.Contains(s, strings.ToLower(m)) {
			return true
		}
	}
	// 业务码兜底：上游 10001 = 今天已签到。用带引号/等号的精确形式，
	// 避免 requestId 等字段里偶然出现的数字造成误判。
	for _, m := range []string{`"code":10001`, `"code": 10001`, "code=10001"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// alreadyCheckedMarkers 重复签到的文案特征（仅业务层文案）。
//
// 刻意【不】收录的判据（均为历史缺陷根因，2026-09-13 修正）：
//   - "checkin"：URL 路径里就有（/v2/billing/meter/daily-checkin），
//     导致任何签到请求的网络错误都被误判为「今天已签到」——真实失败被伪装成成功；
//   - "code=400"：任何 HTTP 400 都会被误判（重复签到只是 400 的一种）；
//   - 单独的 "already"：过于宽泛，可能命中无关文案。
var alreadyCheckedMarkers = []string{
	"已签到",
	"already checked",
	"already checkin",
}
