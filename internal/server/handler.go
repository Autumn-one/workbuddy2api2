// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权
	MaxRotate int    // 单请求最多尝试账号次数，默认 5
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429 冷却，默认 60s
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// UsageStore token 用量统计（账号×模型×日期）；nil = 不统计。
	// 仅观测，不参与任何决策。
	UsageStore *TokenUsageStore
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 5
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imageGenerations))
	h.mux.HandleFunc("POST /v1/images/edits", h.withAuth(h.imageEdits))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"healthy": healthy, "total": total})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "name": "kimi-k3-origin"},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
// ids = cli 可对话列表（/v1/models 默认输出）；all = 上游全量目录（?all=1 输出），
// 二者同一次上游请求产出、同一 TTL。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	all      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
// ?all=1 时返回上游全量目录（含非 cli 分组与 disabled 条目），每条额外带
// agents / disabled / callable 标记——仅展示用，callable=false 的模型
// 走 chat 不会被上游接受为对话模型。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("all"); q == "1" || strings.EqualFold(q, "true") {
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   h.modelListAll(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
//
// 除 OpenAI 标准字段外，额外透出上游提供的扩展信息（reasoning / 能力标志 /
// 消耗倍率）。这些是【增量字段】，不影响标准客户端解析；需要思考深度档位的
// 客户端可据此决定传哪个 reasoning_effort。
func (h *Handler) modelList() []map[string]any {
	if infos, _ := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			out = append(out, modelEntry(mi))
		}
		return out
	}
	return staticModels
}

// modelListAll /v1/models?all=1 的全量视图：上游目录里所有模型，
// 在标准条目之上追加 agents（所属分组）、disabled、callable（能否走本网关对话）。
// callable=false 的条目仅用于观测上游目录，客户端不应拿它们发 chat。
func (h *Handler) modelListAll() []map[string]any {
	_, all := h.fetchDynamicModels()
	if len(all) == 0 {
		return nil
	}
	inCLI := func(mi upstream.ModelInfo) bool {
		for _, g := range mi.Agents {
			if g == "cli" {
				return true
			}
		}
		return false
	}
	out := make([]map[string]any, 0, len(all))
	for _, mi := range all {
		entry := modelEntry(mi)
		agents := mi.Agents
		if agents == nil {
			agents = []string{} // nil 切片会序列化成 null，空数组更符合"无分组"语义
		}
		entry["agents"] = agents
		entry["disabled"] = mi.Disabled
		entry["callable"] = inCLI(mi) && !mi.Disabled
		out = append(out, entry)
	}
	return out
}

// modelEntry 单个模型的 OpenAI 兼容条目 + 本网关扩展字段（reasoning/capabilities/倍率）。
func modelEntry(mi upstream.ModelInfo) map[string]any {
	entry := map[string]any{
		"id":                mi.ID,
		"object":            "model",
		"created":           1753600000,
		"owned_by":          "workbuddy",
		"context_length":    mi.ContextWindow,
		"max_output_tokens": mi.MaxTokens,
	}
	if mi.ContextWindow == 0 {
		entry["context_length"] = 131072 // 兜底
	}
	// 模型显示名与简介（上游 descriptionZh/En）。
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.DescriptionZh != "" {
		entry["description"] = mi.DescriptionZh
	} else if mi.DescriptionEn != "" {
		entry["description"] = mi.DescriptionEn
	}
	// 思考深度：默认档 + 可选档 + 能否关闭。
	// 上游对固定单档模型给 defaultEffort，对可调档模型给 supportedEfforts（互斥）。
	reasoning := map[string]any{}
	if mi.DefaultEffort != "" {
		reasoning["default_effort"] = mi.DefaultEffort
	}
	if len(mi.Efforts) > 0 {
		reasoning["supported_efforts"] = mi.Efforts
	}
	if mi.CanDisableThinking {
		reasoning["can_disable_thinking"] = true
	}
	if mi.OnlyReasoning {
		reasoning["only_reasoning"] = true
	}
	if len(reasoning) > 0 {
		entry["reasoning"] = reasoning
	}
	// 能力标志。
	caps := map[string]any{}
	if mi.SupportsImages {
		caps["images"] = true
	}
	if mi.SupportsToolCall {
		caps["tool_calls"] = true
	}
	if mi.SupportsReasoning {
		caps["reasoning"] = true
	}
	// 生图能力：按上游 tags 标记（text-to-image → /v1/images/generations；
	// image-to-image/image-edit → /v1/images/edits）。仅 ?all=1 视图可见——
	// 这类模型不在 cli 组，正常 /v1/models 列表不含它们。
	for _, t := range mi.Tags {
		switch t {
		case "text-to-image":
			caps["image_generation"] = true
		case "image-to-image", "image-edit":
			caps["image_edit"] = true
		}
	}
	if len(caps) > 0 {
		entry["capabilities"] = caps
	}
	// 消耗倍率（非额度）：积分池按此倍率折算各模型可用量。
	if mi.CreditsRate > 0 {
		entry["credits_multiplier"] = mi.CreditsRate
	}
	return entry
}

// fetchDynamicModels 从池中任一健康账号拉模型目录（含 contextWindow/maxTokens），缓存 1h。
// 一次上游请求同时产出 cli 可对话列表与全量目录（后者供 ?all=1 展示）。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels() (cli []upstream.ModelInfo, all []upstream.ModelInfo) {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out, allOut := dynamicModelsCache.ids, dynamicModelsCache.all
		dynamicModelsCache.RUnlock()
		return out, allOut
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil, nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil, nil
	}
	infos, allInfos, err := h.cfg.Upstream.FetchCatalog(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持全局负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil, nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.all = allInfos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos, allInfos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	st.usageSink = h.cfg.UsageStore
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}

	// 本请求的模型：6004 按模型冷却的键，选号时按（账号×模型）过滤。
	model := st.model

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满 + 模型冷却），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUID(stickyUID, model)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满/该模型正被 6004 冷却）→ 解绑，本次回落普通轮换。
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcluding(tried, model)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.acct = acct
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				h.cfg.Session.Unbind(sessKey)
				stickyUID = ""
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("chat refresh acct=%s: save auth failed: %v", logAccountName(acct), err)
			}
		}

		rc, status, respBody, params, terr := h.cfg.Upstream.ChatStreamWithParams(acct, body)
		// 实际发往上游的关键参数：无论成败都记下（失败行同样需要"请求了什么"才能排查）。
		st.params = params
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			// 上下文超限（11115）：确定性失败，与账号无关——不再轮转，立即原样上报。
			//
			// 为什么不换号：请求体自身超出模型上限，换任何账号都是同一个 body、
			// 同一样被拒（生产实测：11 个请求各连撞 5 次全败，每次白耗 ~30 秒）。
			// 为什么不做账号处置：这不是账号故障，冷却/熔断会误伤好账号。
			//
			// 上报方式：原样透传上游 body（含 displayMsg 多语言文案），并保留上游
			// 状态码。客户端（如 pi）靠 extError.code=context_length_exceeded 或
			// 文案特征识别溢出，才能触发"压缩上下文后重试"的自动恢复。
			//
			// 注意：此处【不调用】fail(acct.UID)，仅释放在途租约（否则该账号名额
			// 会被本请求一直占着到 next 轮转）。
			if kind == upstream.ErrContextOverflow {
				releaseHeld()
				h.writeUpstreamError(w, status, respBody)
				return
			}
			h.applyErrorPolicy(acct, st.model, kind, status, string(respBody))
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = upstream.Stream(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			st.inTok = stats.PromptTokens()
			st.thinkTok = stats.ThinkingTokens()
			st.cachedTok = stats.CachedTokens()
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败（畸形 SSE / 无有效数据帧）：
			// 此时客户端还没看到任何输出，所以可以安全地换号重试。
			// 不喂熔断器、不禁用账号（流格式问题不代表账号故障）。
			lastErr = &parseFailure{err: err}
			fail(acct.UID)
			continue
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		st.inTok = promptTokens(resp)
		st.thinkTok = thinkingTokens(resp)
		st.cachedTok = cachedTokens(resp)
		return
	}
	// 轮转耗尽的出口分两类：
	//   - 账号都不可用（冷却/禁用/模型限流）→ 503 no_healthy_account
	//   - 账号健康但上游返回的流格式坏了（解析失败）→ 502 upstream_parse
	// 区分的价值：客户端对两者处理不同（503 是等账号恢复，502 是上游数据问题）。
	if isParseFailure(lastErr) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", lastErr.Error())
		st.status = http.StatusBadGateway
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// modelRateCooldownBase 6004 模型级限流的起步冷却时长（用户指定 10 分钟）；
// 连续撞墙翻倍、封顶 maxModelCooldown（60 分钟），见 pool.NoteModelRateLimit。
const modelRateCooldownBase = 10 * time.Minute

// contentRejectCooldown 内容安全拒绝（code=11140）的【基础】冷却时长。
// 连续次数达 pool.suspectBanThreshold 后升级为疑似拉黑长冷却（1 小时）。
//
// 为什么是短期而非禁用：生产实证（2026-09-12）显示某账号 2 小时内 189 次
// 全被 11140 拒绝、成功 0 次，但风控标记通常是临时的；禁用会让账号永久退场，
// 误伤风险大。冷却到期自动恢复，也可在「测试」页手动复测确认是否已解除。
const contentRejectCooldown = 10 * time.Minute

// truncateMsg 截断报文用于日志（避免长报文刷屏）。
func truncateMsg(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// parseFailure 流解析失败的包装类型。
//
// 为什么要专门包一层：传输层错误（连不上/超时）与解析失败都是"非 upstream.Error"，
// 但语义完全不同——传输错误是网络问题（应 503 让客户端重试），解析失败是上游
// 返回了坏数据（应 502 告知）。此前用"不是 upstream.Error"一条判据会把传输错误
// 误报成 502（实测回归：TestChatTransportErrorDoesNotPenalize 失败）。
type parseFailure struct{ err error }

func (e *parseFailure) Error() string { return e.err.Error() }
func (e *parseFailure) Unwrap() error { return e.err }

// isParseFailure 报告 lastErr 是否为「上游流解析失败」。
func isParseFailure(err error) bool {
	var pf *parseFailure
	return errors.As(err, &pf)
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// status/respBody 仅供 ErrModelRateLimit 记录观测证据用，其余分支不使用。
//
// 六条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate / ErrNotFound → Cooldown(CoolSoft)：即时软冷却（429/404）。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrModelRateLimit → 按【账号×模型】短冷却（10 分钟起、翻倍、封顶 60 分钟）：
//     不做账号级处置、不喂熔断；不采信报文里的重置时刻。
//   - ErrContextOverflow → 【不会走到这里】：caller 在调用前已 return（不换号、
//     不罚账号、原样上报上游 body）。保留在 switch 外是为了让"不做处置"显式可见。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(acct *auth.Auth, model string, kind upstream.ErrKind, status int, respBody string) {
	uid := acct.UID // 冷却/熔断状态机的键仍是 UID；日志展示用 logAccountName(acct)
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrModelRateLimit:
		// 模型级频率限制（code 6004）：按【账号×模型】短冷却——起步 10 分钟、
		// 连续撞墙翻倍、封顶 60 分钟。刻意【不采信报文里的重置时刻】（实测预报
		// 22:56 实际 02:52 已恢复）。也不做账号级处置：该限制是模型级的，
		// 同账号其他模型实测照常可用，账号级冷却会造成长时间误伤。
		//
		// 动机（生产日志实测）：不冷却时同一账号同模型间隔 1~2 分钟被反复选中
		// 反复撞墙（02:41→03:20 撞 7 次），既浪费上游往返也加重限流。
		uid := acct.UID
		if model == "" {
			// 请求体缺 model：无从建立（账号×模型）键，仅记录不冷却。
			log.Printf("model_rate_limit acct=%s status=%d msg=%s (请求无 model 字段，未冷却)", logAccountName(acct), status, truncateMsg(respBody))
			break
		}
		d := h.cfg.Pool.NoteModelRateLimit(uid, model, modelRateCooldownBase)
		if ev, ok := upstream.ParseModelRateLimitFromMsg(status, respBody); ok && ev.Model != "" && ev.Model != model {
			// 报文点名的模型 ≠ 本请求模型：仍按【请求模型】冷却（发出去的就是它），
			// 但把差异亮出来，便于发现"上游把别的模型的额度记到这个请求头上"类异常。
			log.Printf("model_rate_limit acct=%s model=%s(请求) vs %s(报文) 冷却=%s", logAccountName(acct), model, ev.Model, d)
			break
		}
		log.Printf("model_rate_limit acct=%s model=%s 冷却=%s status=%d", logAccountName(acct), model, d, status)
	case upstream.ErrContentRejected:
		// 内容安全拒绝（11140）：给【该账号】短期冷却。
		//
		// 关键判断依据（生产实证，见 upstream.ErrContentRejected 注释）：
		// 同一个请求体，被标记的账号必被拒、换其他账号立刻 200——
		// 所以这不是内容问题，而是账号被上游风控标记。
		// 若按"客户端错误只换号不罚"处理，被标记账号会反复被选中、每次白撞
		// 一次上游往返（实测 189 次），既浪费轮换也让请求多一跳延迟。
		//
		// 不喂熔断、不禁用：账号本身健康（上游明确说是内容问题），
		// 喂熔断会把好账号熔断掉；禁用则误伤过重（风控多为临时）。
		// 用 NoteContentReject 而非 Cooldown：账号是健康的（上游明确是内容审核问题），
		// 喂熔断会把它错误地熔断（指数退避最长数小时），误伤过重。
		// 连续达阈值会升级为"疑似拉黑"长冷却（见 pool.suspectBanThreshold）——
		// 实测某些账号被标记后持续 11140（189 次全拒），只给 10 分钟冷却会让它
		// 反复被抽中白撞，足以把轮转次数耗光。
		d, suspected := h.cfg.Pool.NoteContentReject(uid, contentRejectCooldown)
		if suspected {
			log.Printf("content_rejected acct=%s model=%s 冷却=%s status=%d（连续 %d 次被拒，判定疑似被上游拉黑；"+
				"冷却到期会自动给一次重试机会，或在 GUI 手动解除）",
				logAccountName(acct), model, d, status, h.cfg.Pool.ContentRejectCount(uid))
			break
		}
		log.Printf("content_rejected acct=%s model=%s 冷却=%s status=%d（该账号被上游风控标记，换号重试）",
			logAccountName(acct), model, d, status)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// writeUpstreamError 以【OpenAI 错误信封】返回上游错误，同时保留上游原始字段。
//
// 为什么不能直接原样透传上游 body（实测教训 2026-09-13 05:44）：
// openai SDK 在非 2xx 时只读 body 的 `error` 键（core/error.mjs 的
// `errorResponse?.['error']`），取不到就丢掉整个 body，报
// "400 status code (no body)"。上游 body 用的是自有形状
// {code,msg,extError,displayMsg}，没有 `error` 键——所以原样透传反而让
// 客户端拿不到任何信息，pi 连 "context_length_exceeded" 都看不到。
//
// 因此这里同时输出两套字段：
//   - error.{message,type,code}：OpenAI 标准信封，供 SDK 解析；
//   - code/msg/extError/displayMsg：上游原始字段，保留给能识别它们的客户端。
//
// message 开头【必须】带上游 extError.code（"context_length_exceeded"）：
// pi 的溢出识别读的是 errorMessage（实现在 pi-ai/utils/overflow.js），
// 只认模式串。把该码放在行首，"上下文超限" 才能被识别并触发压缩恢复。
func (h *Handler) writeUpstreamError(w http.ResponseWriter, status int, body []byte) {
	out := map[string]any{}
	// 保留上游原始字段（无损：解析失败就退化为空信封，不报错）。
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err == nil && raw != nil {
		for k, v := range raw {
			out[k] = v
		}
	}
	// 组装 OpenAI 信封；message 以可识别的溢出码开头。
	msg := "upstream error"
	if ev, ok := upstream.ParseContextOverflowMsg(status, string(body)); ok && ev.Code != "" {
		msg = ev.Code
		if ev.Text != "" {
			msg += ": " + ev.Text
		}
	} else if s, ok := raw["msg"].(string); ok && s != "" {
		msg = s
	}
	typ := "api_error"
	if ext, ok := raw["extError"].(map[string]any); ok {
		if t, ok := ext["type"].(string); ok && t != "" {
			typ = t
		}
	}
	out["error"] = map[string]any{
		"message": msg,
		"type":    typ,
		"code":    "context_length_exceeded",
	}
	writeJSON(w, status, out)
}
