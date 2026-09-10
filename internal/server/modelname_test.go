package server

import (
	"strings"
	"testing"
)

// TestPadModelNameKeepsModelsDistinguishable 回归测试：日志模型名必须能区分不同模型。
//
// 缺陷背景：原实现硬截断到 11 字符，导致 deepseek-v4.1-flash / deepseek-v4-flash /
// deepseek-v4-pro 三个不同模型全部显示为 "deepseek-v4"，日志丧失排障价值。
func TestPadModelNameKeepsModelsDistinguishable(t *testing.T) {
	// 这三个必须互不相同——这是本次修复的核心诉求
	names := []string{"deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-pro"}
	seen := map[string]string{}
	for _, n := range names {
		got := padModelName(n)
		if prev, dup := seen[got]; dup {
			t.Errorf("模型 %q 与 %q 显示为同一名字 %q —— 日志无法区分", prev, n, got)
		}
		seen[got] = n
	}

	// 实测上游全部模型 ID 都应完整显示（不截断）
	all := []string{
		"deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-pro", "deepseek-v3-2-volc",
		"glm-5.3-flash", "glm-5v-turbo", "kimi-k2-thinking", "kimi-k3-1", "minimax-m2.5",
		"hunyuan-2.0-thinking", "hunyuan-image-v3.0", "hy4-preview", "hy3-x", "auto", "default",
	}
	for _, n := range all {
		if got := padModelName(n); got != n {
			t.Errorf("模型 %q 显示为 %q —— 应完整显示", n, got)
		}
	}
}

// TestPadModelNameTruncationVisible 超长模型名应加省略号，使"被截断"可见。
func TestPadModelNameTruncationVisible(t *testing.T) {
	long := strings.Repeat("x", 30)
	got := padModelName(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("超长模型名应带省略号提示，got %q", got)
	}
	if len([]rune(got)) != modelColWidth {
		t.Errorf("截断后长度=%d want %d", len([]rune(got)), modelColWidth)
	}
}

// TestPadModelNameEmpty 空模型名回落 "-"（与既有行为一致）。
func TestPadModelNameEmpty(t *testing.T) {
	if got := padModelName(""); got != "-" {
		t.Errorf("空模型名应显示 -, got %q", got)
	}
}

// TestLogChatRowShowsFullModel 端到端：日志行里出现完整模型名，不再是截断的 "deepseek-v4"。
func TestLogChatRowShowsFullModel(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, 0, "deepseek-v4.1-flash", "stream", "uid-12345678", 200, -1)
	})
	if !strings.Contains(out, "deepseek-v4.1-flash") {
		t.Errorf("日志应含完整模型名 deepseek-v4.1-flash:\n%s", out)
	}
}
