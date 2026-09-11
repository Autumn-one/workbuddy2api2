package main

import (
	"testing"
)

// ─────────────── 优先级输入解析（UI 层）───────────────
//
// 需求：每个账号可单独设置权重值（不再只有固定的 ×3）。
// 输入框接受任意正浮点数；0/空 = 取消设置；非法输入必须给明确提示。

// TestParsePriorityInput 输入解析规则。
func TestParsePriorityInput(t *testing.T) {
	cases := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"3", 3, true},
		{"0.5", 0.5, true},
		{"7.25", 7.25, true},
		{"10", 10, true},
		{"0", 0, true}, // 0 = 取消设置
		{"  2.5  ", 2.5, true},
		{"abc", 0, false},
		{"", 0, false}, // 空 → 提示用户填写（不静默当 0）
		{"-1", 0, false},
		{"1e2", 100, true}, // 科学计数法 ParseFloat 支持，可接受
	}
	for _, c := range cases {
		v, ok := parsePriorityInput(c.in)
		if ok != c.wantOK {
			t.Errorf("parsePriorityInput(%q) ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && v != c.want {
			t.Errorf("parsePriorityInput(%q)=%v want %v", c.in, v, c.want)
		}
	}
}
