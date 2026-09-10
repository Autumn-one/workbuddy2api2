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

// ModelRateLimitEvidence 模型级频率限制的观测证据（仅记录，不参与调度）。
type ModelRateLimitEvidence struct {
	Status int    // HTTP 状态码
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

// ParseModelRateLimitFromMsg 从报文文本提取模型级频率限制证据。
// 返回 ok=false 表示不是该类限制。仅用于记录，不解析"重置时间"
// （实测该时间不可信，故刻意不提取，避免下游误用）。
func ParseModelRateLimitFromMsg(status int, msg string) (ModelRateLimitEvidence, bool) {
	if !isModelRateLimit(msg) {
		return ModelRateLimitEvidence{}, false
	}
	return ModelRateLimitEvidence{Status: status, Msg: truncate(msg, 300)}, true
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
func (c *Client) prepareBody(body []byte) []byte {
	return PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints, c.effortsSnapshot())
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
	resp, err := c.HTTP.Do(req)
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
	data, err := c.doJSON(req)
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
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(c.prepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	ChatHeaders(req, a)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s",
			a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
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
	resp, err := c.HTTP.Do(req)
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
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				Credits         string `json:"credits"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
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
	dynMap := make(map[string]struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Credits         string
		Efforts         []string
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = struct {
			ID              string
			Name            string
			MaxInputTokens  int64
			MaxOutputTokens int64
			Disabled        bool
			Credits         string
			Efforts         []string
		}{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Credits, m.Reasoning.SupportedEfforts}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		rate, _ := ParseCreditsRate(m.Credits)
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Efforts,
			CreditsText:   m.Credits,
			CreditsRate:   rate,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Lock()
	c.efforts = cache
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
	data, err := c.doJSON(req)
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
	_, err = c.doJSON(req)
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
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}
