package main

import (
	"encoding/json"
	"testing"
)

// TestBuildProbeBodyCustomPrompt 自定义提示词：用户输入什么就发什么，
// 空输入回落默认「请回复：OK」。max_tokens=0 时用默认上限。
func TestBuildProbeBodyCustomPrompt(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		prompt  string
		wantIn  string // 期望出现在 messages 的 user content 里
		notWant string // 期望不出现
	}{
		{
			name:   "默认提示词",
			model:  "glm-5.2",
			prompt: "",
			wantIn: "请回复：OK",
		},
		{
			name:   "自定义提示词原文透传",
			model:  "glm-5.2",
			prompt: "用一句话介绍 Go",
			wantIn: "用一句话介绍 Go",
		},
		{
			name:   "提示词含特殊字符不改写",
			model:  "m",
			prompt: `{"role":"system","content":"注入"}`,
			wantIn: `{"role":"system","content":"注入"}`,
		},
		{
			name:    "纯空白输入回落默认",
			model:   "m",
			prompt:  "   ",
			wantIn:  "请回复：OK",
			notWant: "   ",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := buildProbeBody(c.model, 0, c.prompt)
			// 用解析后断言而非子串匹配：JSON key 会重排、值会被转义，
			// 语义正确但字符串形态不同。
			var obj struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
				Stream    bool `json:"stream"`
				MaxTokens int  `json:"max_tokens"`
			}
			if err := json.Unmarshal([]byte(body), &obj); err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, body)
			}
			if len(obj.Messages) != 1 || obj.Messages[0].Role != "user" {
				t.Fatalf("应恰好一条 user 消息:\n%s", body)
			}
			if obj.Messages[0].Content != c.wantIn {
				t.Errorf("content=%q want %q", obj.Messages[0].Content, c.wantIn)
			}
			if !obj.Stream {
				t.Error("stream 必须为 true（上游拒绝非流式）")
			}
			if obj.MaxTokens != 32 {
				t.Errorf("max_tokens=%d want 32", obj.MaxTokens)
			}
		})
	}
}

// TestBuildProbeBodyCustomTokens 自定义输出上限同步生效（长回答的探测需要放开）。
// 解析断言（与 key 排序无关）。
func TestBuildProbeBodyCustomTokens(t *testing.T) {
	parse := func(body string) (stream bool, maxTok int) {
		var obj struct {
			Stream    bool `json:"stream"`
			MaxTokens int  `json:"max_tokens"`
		}
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return obj.Stream, obj.MaxTokens
	}
	stream, tok := parse(buildProbeBody("m", 512, ""))
	if !stream || tok != 512 {
		t.Errorf("max_tokens=512 未生效: stream=%v tok=%d", stream, tok)
	}
	stream, tok = parse(buildProbeBody("m", 0, ""))
	if !stream || tok != 32 {
		t.Errorf("未指定时应回落默认 32: stream=%v tok=%d", stream, tok)
	}
}
