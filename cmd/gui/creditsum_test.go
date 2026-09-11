package main

import (
	"testing"

	"workbuddy2api/internal/pool"
)

// ─────────────── 账号页总积分统计 ───────────────
//
// 关键：必须区分「未知」与「真的是 0」。
// 账号页的「积分」列在 credits==0 时显示 "-"（从未刷新过额度 → 未知），
// 但 Status 原先不暴露 creditsKnown，若直接求和会把"未知"当 0 混进去，
// 总数静默偏低（用户看到的总数比实际少，且没有任何提示）。

// TestSumCreditsBasic 基本求和。
func TestSumCreditsBasic(t *testing.T) {
	items := []pool.Status{
		{UID: "a", Credits: 100, CreditsKnown: true},
		{UID: "b", Credits: 250, CreditsKnown: true},
		{UID: "c", Credits: 30, CreditsKnown: true},
	}
	got, unknown, known := sumCredits(items)
	if got != 380 {
		t.Errorf("总数=%d want 380", got)
	}
	if unknown != 0 || known != 3 {
		t.Errorf("unknown=%d known=%d want 0/3", unknown, known)
	}
}

// TestSumCreditsSkipsUnknown 「未知」不得计入总数，且要单独报出数量。
func TestSumCreditsSkipsUnknown(t *testing.T) {
	items := []pool.Status{
		{UID: "a", Credits: 100, CreditsKnown: true},
		{UID: "b", Credits: 0, CreditsKnown: false}, // 从未刷新过 → 未知，不是 0
		{UID: "c", Credits: 0, CreditsKnown: true},  // 真的是 0（额度用尽）
	}
	got, unknown, known := sumCredits(items)
	if got != 100 {
		t.Errorf("总数=%d want 100（未知不得当 0 计入）", got)
	}
	if unknown != 1 {
		t.Errorf("未知账号数=%d want 1", unknown)
	}
	if known != 2 {
		t.Errorf("已知账号数=%d want 2", known)
	}
}

// TestSumCreditsEmpty 空列表安全。
func TestSumCreditsEmpty(t *testing.T) {
	got, unknown, known := sumCredits(nil)
	if got != 0 || unknown != 0 || known != 0 {
		t.Errorf("空列表应全 0, got %d/%d/%d", got, unknown, known)
	}
}

// TestTotalCreditsText 展示文案：总数 + 账号数；有未知时明确提示，不藏。
func TestTotalCreditsText(t *testing.T) {
	cases := []struct {
		name  string
		items []pool.Status
		want  string
	}{
		{
			name:  "全部已知",
			items: []pool.Status{{Credits: 1000, CreditsKnown: true}, {Credits: 2345, CreditsKnown: true}},
			want:  "总积分 3,345 · 2 个账号",
		},
		{
			name:  "含未知账号要提示",
			items: []pool.Status{{Credits: 1000, CreditsKnown: true}, {Credits: 0, CreditsKnown: false}},
			want:  "总积分 1,000 · 2 个账号（1 个未刷新额度，未计入）",
		},
		{
			name:  "空账号",
			items: nil,
			want:  "总积分 0 · 0 个账号",
		},
		{
			name:  "大数带千分位",
			items: []pool.Status{{Credits: 1234567, CreditsKnown: true}},
			want:  "总积分 1,234,567 · 1 个账号",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := totalCreditsText(c.items); got != c.want {
				t.Errorf("totalCreditsText=%q\n              want %q", got, c.want)
			}
		})
	}
}

// TestFormatThousands 千分位格式化（含负数与边界）。
func TestFormatThousands(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-1234, "-1,234"},
	}
	for _, c := range cases {
		if got := formatThousands(c.in); got != c.want {
			t.Errorf("formatThousands(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestTotalCreditsTextServiceNotStarted 服务未启动时 Accounts() 返回 nil：
// 统计文案必须正常显示 0，不得 panic。这是"启动 GUI 但服务未起"的真实场景。
func TestTotalCreditsTextServiceNotStarted(t *testing.T) {
	svc := NewService() // pool 为 nil → Accounts() 返回 nil
	items := svc.Accounts()
	if items != nil {
		t.Fatalf("未启动的服务应返回空账号列表, got %d", len(items))
	}
	if txt := totalCreditsText(items); txt != "总积分 0 · 0 个账号" {
		t.Errorf("未启动服务下文案=%q want %q", txt, "总积分 0 · 0 个账号")
	}
	// refreshTotalCredits 在 lbl 为 nil 时也必须安全（构造期调用）
	a := &app{svc: svc}
	a.refreshTotalCredits(items) // lblTotalCredits 为 nil → 直接返回
}
