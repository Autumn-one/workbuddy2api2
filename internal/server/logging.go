// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogWriter 请求表格日志的自定义输出目标；nil 表示用 os.Stdout。
// 刻意不在此处缓存 os.Stdout：nil 时在写日志的瞬间再读，这样测试对
// os.Stdout 的临时替换（以及 GUI 接管）都仍然生效。
var chatLogWriter io.Writer

// SetChatLogWriter 更换请求日志输出目标；传 nil 恢复 stdout。
// 非并发安全（只在启动装配阶段调用一次）。
func SetChatLogWriter(w io.Writer) {
	chatLogWriter = w
}

// chatOut 返回当前请求日志输出目标。
func chatOut() io.Writer {
	if chatLogWriter != nil {
		return chatLogWriter
	}
	return os.Stdout
}

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start  time.Time
	model  string
	mode   string     // "stream" | "sync"
	acct   *auth.Auth // 完整账号（展示时取昵称前 7 字符，无昵称回落 UID 前 8 位）
	ttfb   time.Duration
	toks   int // <0 表示 usage 缺失 → 显示 "-"
	status int

	// params 实际发往上游的关键参数（思考档位 / 输出上限），由 ChatStreamWithParams 回填。
	// 零值 = 未取到（如未走到上游调用就失败），日志显示 "-"。
	params upstream.EffectiveParams

	// inTok 输入（上下文）token 用量，来自末尾 usage 帧的 prompt_tokens；<0 = 缺失。
	inTok int
	// thinkTok 思考（推理）token 用量，来自 usage 的 reasoning_tokens / completion_thinking_tokens；
	// <0 = 缺失。它是"思考深度档位是否真的生效"的直接证据（比 effort= 更接近事实）。
	thinkTok int
	// cachedTok 命中缓存的输入 token；<0 = 缺失。
	cachedTok int

	// usageSink 用量统计落点（由 server.Config 注入；nil = 不统计）。
	// 放在 chatStat 上而不是全局变量：便于测试隔离，也让"谁在统计"显式可见。
	usageSink *TokenUsageStore

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1, inTok: -1, thinkTok: -1, cachedTok: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.acct, s.status, s.toks, s.inTok, s.cachedTok, s.thinkTok, s.params)
	// 用量统计（账号 × 模型 × 日期）。无论成功失败都计一次请求数；
	// token 在失败请求里为 -1（上游不返回 usage）→ 由 store 钳 0 并计入 Missing。
	s.recordUsage()
}

// recordUsage 把本次请求的用量写入统计存储（未注入 sink 时为空操作）。
func (s *chatStat) recordUsage() {
	if s.usageSink == nil {
		return
	}
	uid := ""
	if s.acct != nil {
		uid = s.acct.UID
	}
	s.usageSink.Record(uid, s.model, s.start, TokenDelta{
		In:     s.inTok,
		Out:    s.toks,
		Think:  s.thinkTok,
		Cached: s.cachedTok,
	})
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br       *bufio.Reader
	start    time.Time
	ttfb     time.Duration
	seen     bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage bool // 末帧是否带 usage
	tokens   int
	// hasPrompt/promptTok 输入（上下文）token 用量：usage.prompt_tokens。
	// 单独用 bool 标记存在性：0 是合法值，不能拿 0 当"缺失"。
	hasPrompt bool
	promptTok int
	// hasThink/thinkTok 思考（推理）token 用量。上游同时提供两个字段，两者同源：
	//   - completion_tokens_details.reasoning_tokens（OpenAI 标准字段）
	//   - completion_thinking_tokens（上游自有字段）
	// 优先取标准字段，缺失时回落自有字段；只采信上游上报值，不做任何估算。
	hasThink bool
	thinkTok int
	// hasCached/cachedTok 命中缓存的输入 token（cached_tokens / cache_read_input_tokens）。
	// 单独统计的意义：ctx 很大时若大部分命中缓存，实际计费输入远小于 ctx——
	// 这是"token 花在哪"里最容易被误读的一项，必须能看到。
	hasCached bool
	cachedTok int
	pend      []byte // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// PromptTokens 返回输入（上下文）token 数，即 usage.prompt_tokens；缺失返回 -1。
// 这是判断"上下文到底占多少"的唯一可信来源，不做任何本地估算。
func (s *chatStatsReader) PromptTokens() int {
	if !s.hasPrompt {
		return -1
	}
	return s.promptTok
}

// ThinkingTokens 返回思考（推理）token 数；缺失返回 -1。
func (s *chatStatsReader) ThinkingTokens() int {
	if !s.hasThink {
		return -1
	}
	return s.thinkTok
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 token 数。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage *struct {
			CompletionTokens int `json:"completion_tokens"`
			// 用指针区分"缺该字段"（未知）与"真是 0"（合法的空上下文）。
			PromptTokens *int `json:"prompt_tokens"`
			// 思考 token：上游自有字段（同样的理由用指针）。
			ThinkingTokens *int `json:"completion_thinking_tokens"`
			// 思考 token：OpenAI 标准嵌套字段。
			Details *struct {
				ReasoningTokens *int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			// 命中缓存的输入 token：上游给两个同源字段，优先标准字段。
			CachedTokens    *int `json:"cached_tokens"`
			CacheReadTokens *int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	if chunk.Usage.PromptTokens != nil {
		s.hasPrompt = true
		s.promptTok = *chunk.Usage.PromptTokens
	}
	// 标准字段优先；两者都存在时取标准字段（与上游实测同值，不会互相矛盾）。
	if d := chunk.Usage.Details; d != nil && d.ReasoningTokens != nil {
		s.hasThink = true
		s.thinkTok = *d.ReasoningTokens
	} else if chunk.Usage.ThinkingTokens != nil {
		s.hasThink = true
		s.thinkTok = *chunk.Usage.ThinkingTokens
	}
	// 缓存命中的输入：cached_tokens 优先，回落 cache_read_input_tokens（同源字段）。
	if chunk.Usage.CachedTokens != nil {
		s.hasCached = true
		s.cachedTok = *chunk.Usage.CachedTokens
	} else if chunk.Usage.CacheReadTokens != nil {
		s.hasCached = true
		s.cachedTok = *chunk.Usage.CacheReadTokens
	}
}

// CachedTokens 返回命中缓存的输入 token 数；缺失返回 -1。
func (s *chatStatsReader) CachedTokens() int {
	if !s.hasCached {
		return -1
	}
	return s.cachedTok
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	return usageInt(resp, "completion_tokens")
}

// promptTokens 从 Aggregate 返回的响应中提取 usage.prompt_tokens（输入/上下文用量）；缺失返回 -1。
func promptTokens(resp map[string]any) int {
	return usageInt(resp, "prompt_tokens")
}

// cachedTokens 从 Aggregate 返回的响应中提取命中缓存的输入 token；缺失返回 -1。
// 优先 cached_tokens，回落 cache_read_input_tokens（同源字段）。
func cachedTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	if v, ok := u["cached_tokens"].(float64); ok {
		return int(v)
	}
	if v, ok := u["cache_read_input_tokens"].(float64); ok {
		return int(v)
	}
	return -1
}

// thinkingTokens 从 Aggregate 返回的响应中提取思考（推理）token 数；缺失返回 -1。
// 优先 OpenAI 标准字段 usage.completion_tokens_details.reasoning_tokens，
// 缺失时回落上游自有字段 usage.completion_thinking_tokens。只采信上游上报值。
func thinkingTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		if v, ok := d["reasoning_tokens"].(float64); ok {
			return int(v)
		}
	}
	if v, ok := u["completion_thinking_tokens"].(float64); ok {
		return int(v)
	}
	return -1
}

// usageInt 取 resp.usage.<key> 的整数值；缺失/类型不符返回 -1。
func usageInt(resp map[string]any, key string) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u[key].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// logAccountName 生成日志/表格里的账号标识：昵称前 7 字符优先，无昵称回落 UID 前 8 位。
//
// 为什么优先昵称：日志是给人看的，昵称（GUI 里配置的账号名）比一串 UID 好认得多；
// 保留 UID 回落是因为昵称可能为空（手工放置的凭证文件），此时退回 UID 仍可定位账号。
// 昵称取前 7 字符是为了控制列宽（中文昵称 7 字已足够区分，且比 8 位 UID 更短）。
func logAccountName(a *auth.Auth) string {
	if a == nil {
		return "-"
	}
	name := strings.TrimSpace(a.Nickname)
	if name != "" {
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

// modelColWidth 模型列的显示宽度。
//
// 修复说明：原实现硬截断到 11 字符，导致多个不同模型塌缩成同一个名字——
// "deepseek-v4.1-flash" / "deepseek-v4-flash" / "deepseek-v4-pro" 全部显示为
// "deepseek-v4"，日志完全丧失区分能力（排障时无法判断实际调用的是哪个模型）。
// 现放宽到 20 字符（覆盖实测全部模型 ID），并对超长者加省略号提示而非静默截断。
const modelColWidth = 20

// padModelName 规范化模型名用于定宽表格显示。
// 超长时保留前缀 + "…"，使"被截断"这件事在视觉上可见，不再静默变名。
func padModelName(model string) string {
	if model == "" {
		return "-"
	}
	if len(model) > modelColWidth {
		// 按 rune 安全截断（模型 ID 目前均为 ASCII，但仍避免切裂多字节字符）。
		rs := []rune(model)
		if len(rs) > modelColWidth-1 {
			rs = rs[:modelColWidth-1]
		}
		return string(rs) + "…"
	}
	return model
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks/inTok <0 表示 usage 缺失，显示 "-"；params 零值字段同样显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode string, acct *auth.Auth, status int, toks, inTok, cachedTok, thinkTok int, params upstream.EffectiveParams) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = padModelName(model)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(chatOut(), "| #%03d | %s | %s | %s | %d | acct=%s | proxy=%s | %s | ctx=%s | cache=%s | TTFB=%s | tok=%s | think=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		logAccountName(acct),
		upstream.ProxyLabelForLog(params.Proxy),
		paramsText(params),
		intOrDash(inTok),
		intOrDash(cachedTok),
		ttfbMS,
		tokField,
		intOrDash(thinkTok),
		tokpsField,
		total.Seconds(),
	)
}

// intOrDash 把 token 计数渲染为字段文案；负数（usage 缺失）显示 "-"。
func intOrDash(n int) string {
	if n < 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

// paramsText 把关键请求参数压成定宽短文案，形如 "effort=high max=8192"。
//
// 只输出【真正会影响上游行为】且【排查时真正需要】的参数：
//   - effort：思考深度档，直接决定思考 token 量与开销（不传显示 effort=-）；
//     发生降级时显示为 effort=high←max（←后是客户端原始请求值）。
//   - max=：客户端指定的输出上限（max_tokens / max_completion_tokens），
//     为 0/未给时显示 max=-（此时由上游默认值决定，日志不猜测）。
//   - thinkctl：思考开关类字段摘要（thinking.type / reasoning.effort 嵌套 /
//     enable_thinking 等透传字段），形如 thinking:disabled；未携带显示 -。
//     与 think= 列（上游实际回报的思考 token 数）配合，可核对"关思考"是否真生效。
//
// 上下文大小（prompt_tokens）不在此处，它由 usage 的 ctx= 字段展示：
// 前者是"客户端要求的输出上限"，后者是"上游实际报的输入用量"，两者不可混同。
func paramsText(p upstream.EffectiveParams) string {
	effort := p.Effort
	if effort == "" {
		effort = "-"
	} else if p.Downgraded() {
		// 明确标出被改写：客户端请求 > 实际发出，避免误读为"日志与请求不符"。
		effort = fmt.Sprintf("%s←%s", p.Effort, p.EffortReq)
	}
	maxTok := "-"
	if p.MaxTokens > 0 {
		maxTok = fmt.Sprintf("%d", p.MaxTokens)
	}
	thinkCtl := p.ThinkCtl
	if thinkCtl == "" {
		thinkCtl = "-"
	}
	return fmt.Sprintf("effort=%s max=%s thinkctl=%s", effort, maxTok, thinkCtl)
}
