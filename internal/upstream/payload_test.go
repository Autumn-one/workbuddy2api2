package upstream

import (
	"encoding/json"
	"testing"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts, nil)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestEffectiveParamsMirrorPreparedBody 关键不变量：日志展示的参数必须与【真正发出的报文】一致。
//
// 这是本功能的核心正确性要求——如果日志读的是客户端原始值，那么 reasoning_effort
// 被降级时日志就会与上游实际收到的请求不符，排障时反而误导。
func TestEffectiveParamsMirrorPreparedBody(t *testing.T) {
	efforts := map[string][]string{"glm-5.2-mini": {"low", "medium"}}
	cases := []struct {
		name     string
		body     string
		wantOut  string // 期望实际发出的 effort
		wantReq  string // 期望记录的客户端请求 effort
		wantMax  int
		wantDown bool
	}{
		{
			name: "passthrough supported effort",
			body: `{"model":"glm-5.2-mini","reasoning_effort":"low","max_tokens":4096}`,
			// low 受支持 → 原样；requested == effective → 未降级
			wantOut: "low", wantReq: "low", wantMax: 4096, wantDown: false,
		},
		{
			name: "downgraded effort is visible as such",
			body: `{"model":"glm-5.2-mini","reasoning_effort":"high","max_tokens":1024}`,
			// high 不支持 → 降为 medium；日志须同时保留请求值与实际值
			wantOut: "medium", wantReq: "high", wantMax: 1024, wantDown: true,
		},
		{
			name:    "unknown model passes through",
			body:    `{"model":"mystery","reasoning_effort":"max"}`,
			wantOut: "max", wantReq: "max", wantMax: 0, wantDown: false,
		},
		{
			name:    "camelCase effort field is captured",
			body:    `{"model":"glm-5.2-mini","reasoningEffort":"high"}`,
			wantOut: "medium", wantReq: "high", wantMax: 0, wantDown: true,
		},
		{
			name:    "max_completion_tokens takes precedence over max_tokens",
			body:    `{"model":"glm-5.2","max_tokens":100,"max_completion_tokens":777}`,
			wantOut: "", wantReq: "", wantMax: 777, wantDown: false,
		},
		{
			name:    "no params yields zeros",
			body:    `{"model":"glm-5.2","messages":[]}`,
			wantOut: "", wantReq: "", wantMax: 0, wantDown: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, params := PrepareBodyOptWithEffortsAndParams([]byte(c.body), false, efforts, nil)

			// 1) 参数必须等于改写后报文里的真实字段值。
			var sent map[string]any
			if err := json.Unmarshal(out, &sent); err != nil {
				t.Fatalf("unmarshal prepared body: %v", err)
			}
			sentEffort, _ := sent["reasoning_effort"].(string)
			if sentEffort == "" {
				sentEffort, _ = sent["reasoningEffort"].(string)
			}
			if sentEffort != params.Effort {
				t.Errorf("记录的 effort=%q 与实发报文 %q 不一致（日志会误导排障）", params.Effort, sentEffort)
			}
			if sentEffort != c.wantOut {
				t.Errorf("实发 effort=%q want %q", sentEffort, c.wantOut)
			}

			// 2) 客户端原始请求档位单独保留，便于识别降级。
			if params.EffortReq != c.wantReq {
				t.Errorf("请求 effort=%q want %q", params.EffortReq, c.wantReq)
			}
			if got := params.Downgraded(); got != c.wantDown {
				t.Errorf("Downgraded()=%v want %v", got, c.wantDown)
			}

			// 3) 输出上限。
			if params.MaxTokens != c.wantMax {
				t.Errorf("MaxTokens=%d want %d", params.MaxTokens, c.wantMax)
			}
		})
	}
}

// TestEffectiveParamsDoesNotChangeBody 日志能力必须零副作用：
// 与不含参数的旧 API 产出逐字节相同（否则会悄悄改变发往上游的请求）。
func TestEffectiveParamsDoesNotChangeBody(t *testing.T) {
	efforts := map[string][]string{"glm-5.2-mini": {"low", "medium"}}
	bodies := []string{
		`{"model":"glm-5.2-mini","reasoning_effort":"high","max_tokens":100}`,
		`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"x","tool_choice":{"type":"none"},"tools":[{"a":1}]}`,
		`not json`,
		``,
	}
	for _, b := range bodies {
		oldOut := PrepareBodyOptWithEfforts([]byte(b), true, efforts, nil)
		newOut, _ := PrepareBodyOptWithEffortsAndParams([]byte(b), true, efforts, nil)
		if string(oldOut) != string(newOut) {
			t.Errorf("body changed for %q:\n old=%s\n new=%s", b, oldOut, newOut)
		}
	}
}
