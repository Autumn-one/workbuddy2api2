// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
// uid 仅用于日志标注（哪个账号的请求被改写了）；测试/无账号场景传 ""。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEfforts(src, sanitize, nil, "")
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
// uid 仅用于日志标注（哪个账号的请求被改写了）；测试/无账号场景传 ""。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string, uid string) []byte {
	out, _ := PrepareBodyOptWithEffortsAndParams(src, sanitize, efforts, uid)
	return out
}

// PrepareBodyOptWithEffortsAndParams 与 PrepareBodyOptWithEfforts 行为完全一致，
// 额外返回【改写后】的关键参数快照供请求日志展示。
//
// 解析失败（非 JSON/空体）时原样返回入参，参数为零值（日志显示"-"）——
// 与既有 early-return 行为逐字一致，不新增错误路径。
func PrepareBodyOptWithEffortsAndParams(src []byte, sanitize bool, efforts map[string][]string, uid string) ([]byte, EffectiveParams) {
	if len(src) == 0 {
		return src, EffectiveParams{}
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, EffectiveParams{}
	}
	// 先取客户端原始档位（降级前），供日志区分"请求档"与"实际档"。
	reqEffort := extractEffort(obj)

	obj["stream"] = true
	normalizeToolChoice(obj)
	normalizeRoles(obj, uid)
	normalizeReasoningEffort(obj, efforts, uid)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src, EffectiveParams{}
	}

	params := extractEffectiveParams(obj)
	params.EffortReq = reqEffort
	return out, params
}

// EffectiveParams 一次出站 chat 请求的关键参数快照（仅供请求日志展示，不参与任何决策）。
//
// 取值时机是【改写之后】：展示的必须是真正发往上游的值，而不是客户端原始请求值——
// 否则 reasoning_effort 被降级时日志会与实际上游行为不符。
type EffectiveParams struct {
	// Effort 实际发往上游的思考档位（reasoning_effort / reasoningEffort，已按 supportedEfforts 降级）。
	// 空串 = 客户端未指定（模型走自身默认档）。
	Effort string
	// EffortReq 客户端原始请求档位。仅在发生降级/floored 改写时与 Effort 不同。
	EffortReq string
	// MaxTokens 客户端指定的输出上限（max_tokens / max_completion_tokens）；0 = 未指定。
	MaxTokens int
}

// Downgraded 报告档位是否被改写（降级或 floor）。
func (p EffectiveParams) Downgraded() bool {
	return p.Effort != "" && p.EffortReq != "" && !strings.EqualFold(p.Effort, p.EffortReq)
}

// extractEffort 取 reasoning_effort / reasoningEffort（snake/camel 双字段兼容）；无则空串。
func extractEffort(obj map[string]any) string {
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		v, present := obj[k]
		if !present {
			continue
		}
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
		return ""
	}
	return ""
}

// extractEffectiveParams 从【已改写】的请求对象里取出关键参数。
// 只读不写：绝不因日志需求改动出站请求体。
func extractEffectiveParams(obj map[string]any) EffectiveParams {
	var p EffectiveParams
	p.Effort = extractEffort(obj)
	// 输出上限：优先 max_completion_tokens（OpenAI 新字段），其次 max_tokens。
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if v, ok := toInt(obj[k]); ok {
			p.MaxTokens = v
			break
		}
	}
	return p
}

// toInt 把 JSON 解出的数值（float64）或整型转成 int；非数值/缺失返回 false。
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string, uid string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded uid=%s model=%s %s -> %s", uidPrefixForLog(uid), model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored uid=%s model=%s %s -> %s", uidPrefixForLog(uid), model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any, uid string) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system uid=%s idx=%d", uidPrefixForLog(uid), i)
		}
	}
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}
