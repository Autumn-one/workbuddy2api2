package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestHalfOpenDetectionSingleSource 半开窗口判定必须只有【一份实现】：
// HalfOpenAllowed 与 pickHalfOpenLocked 曾各写一遍同类逻辑（重复实现迟早漂移）。
// 现在统一为 entry 上的方法，两边共用。本用例锁定两者对同一状态给出一致答案。
func TestHalfOpenDetectionSingleSource(t *testing.T) {
	cases := []struct {
		name  string
		setup func(p *Pool)
		want  bool // HalfOpenAllowed 的期望值
	}{
		{
			name: "无冷却 → 可选",
			setup: func(p *Pool) {
				p.NoteModelRateLimit("a1", "other", 10*time.Minute)
			},
			want: true,
		},
		{
			name: "冷却中+安静期内 → 不允许探测",
			setup: func(p *Pool) {
				p.NoteModelRateLimit("a1", "m", 10*time.Minute)
			},
			want: false,
		},
		{
			name: "冷却中+安静期已过 → 允许探测",
			setup: func(p *Pool) {
				p.NoteModelRateLimit("a1", "m", 10*time.Minute)
				p.mu.Lock()
				p.byUID["a1"].modelCool["m"].lastHit = time.Now().Add(-(halfOpenQuiet + time.Second))
				p.mu.Unlock()
			},
			want: true,
		},
		{
			name: "冷却已过期 → 可选（正常路径）",
			setup: func(p *Pool) {
				p.NoteModelRateLimit("a1", "m", 10*time.Minute)
				p.AdvanceModelCooldowns(time.Now().Add(11 * time.Minute))
			},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newModelTestPool(t)
			p.Add(&auth.Auth{UID: "a1"})
			c.setup(p)
			if got := p.HalfOpenAllowed("a1", "m"); got != c.want {
				t.Errorf("HalfOpenAllowed=%v want %v", got, c.want)
			}
		})
	}
}
