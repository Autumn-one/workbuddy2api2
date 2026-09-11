package main

import (
	"strings"
	"testing"
)

// ─────────────────────────── logBuffer 增量读取 ───────────────────────────
//
// 界面日志刷新从「每 1.5s 全量 SetText」改为「只追加新增行」：
// 全量 SetText 会让 EDIT 控件重建横向滚动范围，叠加 ScrollToCaret 强制把光标
// 滚到最后一行最右端（长行时）——横向滚动条因此乱跳，用户手动滚到的位置被冲掉。
// 增量追加保留控件既有滚动状态，横向位置稳定。

// TestLogBufferDrainReturnsOnlyNewLines drain 只返回上次之后的新行。
func TestLogBufferDrainReturnsOnlyNewLines(t *testing.T) {
	b := &logBuffer{}
	b.Write([]byte("line1\nline2\n"))
	got := b.Drain()
	if len(got) != 2 || got[0] != "line1" || got[1] != "line2" {
		t.Fatalf("第一次 drain=%v want [line1 line2]", got)
	}
	// 没有 newline 的残行留在 part，不算完成行
	b.Write([]byte("line3"))
	if got := b.Drain(); len(got) != 0 {
		t.Fatalf("残行不应被 drain 出来: %v", got)
	}
	b.Write([]byte("\nline4\n"))
	got = b.Drain()
	if len(got) != 2 || got[0] != "line3" || got[1] != "line4" {
		t.Fatalf("第二次 drain=%v want [line3 line4]", got)
	}
}

// TestLogBufferDrainEmpty 无新行时 drain 返回空（UI 跳过刷新）。
func TestLogBufferDrainEmpty(t *testing.T) {
	b := &logBuffer{}
	if got := b.Drain(); len(got) != 0 {
		t.Fatalf("空缓冲 drain=%v want 空", got)
	}
	b.Write([]byte("x\n"))
	_ = b.Drain()
	if got := b.Drain(); len(got) != 0 {
		t.Fatalf("已读后 drain 应为空: %v", got)
	}
}

// TestLogBufferTextStillWorks Text() 全量口径保留（兼容既有调用）。
func TestLogBufferTextStillWorks(t *testing.T) {
	b := &logBuffer{}
	b.Write([]byte("a\nb\n"))
	if txt := b.Text(); txt != "a\r\nb" && txt != "a\nb" {
		t.Errorf("Text()=%q", txt)
	}
}

// TestLogBufferDrainRespectsCapacity drain 之后环形容量仍然生效。
func TestLogBufferDrainRespectsCapacity(t *testing.T) {
	b := &logBuffer{}
	for i := 0; i < logLines+50; i++ {
		b.Write([]byte("x\n"))
	}
	b.Drain()
	for i := 0; i < 5; i++ {
		b.Write([]byte("y\n"))
	}
	got := b.Drain()
	if len(got) != 5 {
		t.Fatalf("drain=%d 行 want 5", len(got))
	}
	if txt := b.Text(); !strings.Contains(txt, "y") {
		t.Error("Text 应包含最新写入")
	}
}

// ─────────────────────────── 追加刷新逻辑 ───────────────────────────

// TestShouldFollowTail 判定「是否自动滚到底」：只有用户本来就停在底部才跟随。
// 用户往上翻历史时，新日志不得把视口拽走（这也是横向滚动条被拉动的诱因之一）。
func TestShouldFollowTail(t *testing.T) {
	cases := []struct {
		name     string
		atBottom bool
		want     bool
	}{
		{"停在底部 → 跟随", true, true},
		{"往上翻了 → 不跟随", false, false},
		{"视口在顶 → 不跟随", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldFollowTail(c.atBottom); got != c.want {
				t.Errorf("shouldFollowTail(%v)=%v want %v", c.atBottom, got, c.want)
			}
		})
	}
}

// TestTruncateLongLogLine 超长行截断：6004 报文等 297 字符的行是横向滚动范围
// 忽宽忽窄的源头，写入日志缓冲前先收敛。
func TestTruncateLongLogLine(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := truncateLogLine(long); len([]rune(got)) != maxLogLineRunes+1 {
		t.Errorf("截断后长度=%d want %d（含省略号）", len([]rune(got)), maxLogLineRunes+1)
	}
	if !strings.HasSuffix(truncateLogLine(long), "…") {
		t.Error("截断应以省略号结尾")
	}
	if got := truncateLogLine("short"); got != "short" {
		t.Errorf("短行不应被改: %q", got)
	}
	// 中文按 rune 截断，不出现半个字
	if got := truncateLogLine(strings.Repeat("中", 300)); len([]rune(got)) != maxLogLineRunes+1 {
		t.Errorf("中文截断长度=%d", len([]rune(got)))
	}
}

// TestLogBufferWriteTruncatesLongLines 写入路径就截断，保证 UI/落盘都不再有超宽行。
func TestLogBufferWriteTruncatesLongLines(t *testing.T) {
	b := &logBuffer{}
	b.Write([]byte(strings.Repeat("x", 500) + "\n"))
	got := b.Drain()
	if len(got) != 1 {
		t.Fatalf("drain=%d", len(got))
	}
	if len([]rune(got[0])) > maxLogLineRunes+1 {
		t.Errorf("超长行未被截断: %d 字符", len([]rune(got[0])))
	}
}
