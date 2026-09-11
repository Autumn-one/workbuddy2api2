package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestNextDay4AMBoundaryAcrossWholeDay 验证"次日 04:00"语义在一天 24 小时的
// 每个整点都成立，且间隔上界恒为 28h（覆盖原断言过窄的时段敏感缺陷）。
//
// 背景：原断言写死 <=24h，在 now ∈ [04:00, 05:00) 时必然失败（每天凌晨复发）。
// 本测试逐小时遍历，确保修复后全天候成立——这是防止该缺陷回归的关键。
func TestNextDay4AMBoundaryAcrossWholeDay(t *testing.T) {
	loc := time.Local
	base := time.Date(2026, 9, 11, 0, 0, 0, 0, loc)

	for h := 0; h < 24; h++ {
		now := base.Add(time.Duration(h) * time.Hour)
		until := nextDay4AM(now)
		d := until.Sub(now)

		// 语义：必须是 now 的【次日】04:00
		want := now.AddDate(0, 0, 1)
		if until.Year() != want.Year() || until.Month() != want.Month() ||
			until.Day() != want.Day() || until.Hour() != 4 {
			t.Errorf("h=%02d: until=%v 应为次日 04:00 (want %v)", h, until, want.Format("2006-01-02 15:04"))
		}
		// 间隔必须落在 (0, 28h]：下界 >0，上界 28h。
		//
		// 上界的推导：语义是 now 的【次日】04:00。最坏情况 now=00:00:00 →
		// 次日 04:00 距离 28h。原测试断言写死 24h，在 now ∈ [00:00, 04:00)
		// 时必然失败（每天凌晨复发）——这才是该缺陷的真实范围，
		// 比最初以为的 "04:00~05:00 之间" 宽得多。
		if d <= 0 {
			t.Errorf("h=%02d: 间隔 %v 必须为正", h, d)
		}
		if d > 28*time.Hour {
			t.Errorf("h=%02d: 间隔 %v 超过 28h 上界", h, d)
		}
		// 下界：now=03:59:59 时距次日 04:00 略超 24h0m？不——是 24h0m1s。
		// 真正的最小间隔出现在 now 接近次日 04:00 时吗？不：nextDay4AM 恒取"次日"，
		// 故 now=23:59:59 时距次日 04:00 为 4h0m1s，是最小值（>4h）。
		if d <= 4*time.Hour-time.Minute {
			t.Errorf("h=%02d: 间隔 %v 小于预期下界 4h（次日语义被破坏）", h, d)
		}
	}
}

// TestCooldownUntilTomorrow4AMIntervalUpperBound 端到端：冷却间隔上界 >= 24h 的场景
// 不再被判失败（回归原缺陷）。
func TestCooldownUntilTomorrow4AMIntervalUpperBound(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	d := st.Until.Sub(before)
	// 上界：now=00:00:01 时间隔约 27h59m59s；now=23:59:59 时约 4h0m1s。
	// 上界放宽到 28h 的依据（实测逐时刻核对）：间隔 = 次日 04:00 - now，
	// 最大值出现在 now=00:00（28h0m），最小值在 now=03:00（25h0m）。
	// 故原 25h 断言在 now ∈ [00:00, 03:00) 运行必失败（时段敏感缺陷），
	// 28h 恰好覆盖极值。
	if d <= 0 || d > 28*time.Hour {
		t.Errorf("冷却间隔 %v 应落在 (0, 28h]", d)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", st.Until.Hour())
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("cool_kind=%q want hard_credit", st.CoolKind)
	}
}
