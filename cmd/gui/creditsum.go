// creditsum.go — 账号页的总积分统计。
//
// 纯展示：只读账号池快照求和，不发起任何上游请求、不改变任何状态。
package main

import (
	"fmt"
	"strings"

	"workbuddy2api/internal/pool"
)

// sumCredits 汇总账号积分，返回 (总数, 未知账号数, 已知账号数)。
//
// 「未知」必须排除在总数之外：账号从未刷新过额度时 credits=0 只代表"未知"
// （与账号页积分列显示 "-" 的口径一致），若当 0 求和会让总数静默偏低。
// 未知数量单独返回，供展示层明确提示，绝不藏。
func sumCredits(items []pool.Status) (total int64, unknown, known int) {
	for _, s := range items {
		if !s.CreditsKnown {
			unknown++
			continue
		}
		total += s.Credits
		known++
	}
	return total, unknown, known
}

// totalCreditsText 生成账号页的总积分文案。
// 有未知账号时明确标注"未计入"，避免用户以为总数就是全部。
func totalCreditsText(items []pool.Status) string {
	total, unknown, _ := sumCredits(items)
	text := fmt.Sprintf("总积分 %s · %d 个账号", formatThousands(total), len(items))
	if unknown > 0 {
		text += fmt.Sprintf("（%d 个未刷新额度，未计入）", unknown)
	}
	return text
}

// formatThousands 把整数格式化为带千分位的字符串（0 → "0"，-1234 → "-1,234"）。
// 手写而非引第三方：只此一处需要，逻辑简单且行为可测。
func formatThousands(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
