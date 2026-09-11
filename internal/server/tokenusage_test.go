package server

import (
	"os"
	"testing"
	"time"
)

// ─────────────── Token 用量统计（账号 × 模型 × 日期）───────────────
//
// 需求：记录所有账号、所有模型的 token 数量。
// 维度：账号 × 模型 × 日期（日期必须记，否则无法回答"今天用了多少"）。
// 展示：账号×模型明细 + 账号级汇总（sum）+ 全局汇总（sum）。

// TestTokenUsageRecordAccumulates 同键累加。
func TestTokenUsageRecordAccumulates(t *testing.T) {
	s := NewTokenUsageStore("")
	day := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)

	s.Record("uid1", "glm-5.2", day, TokenDelta{In: 100, Out: 50, Think: 20, Cached: 30})
	s.Record("uid1", "glm-5.2", day, TokenDelta{In: 200, Out: 80, Think: 10, Cached: 5})

	rows := s.Rows()
	if len(rows) != 1 {
		t.Fatalf("同键应合并为 1 行, got %d", len(rows))
	}
	r := rows[0]
	if r.UID != "uid1" || r.Model != "glm-5.2" || r.Day != "2026-09-12" {
		t.Errorf("键错误: %+v", r)
	}
	if r.In != 300 || r.Out != 130 || r.Think != 30 || r.Cached != 35 {
		t.Errorf("累加错误: In=%d Out=%d Think=%d Cached=%d want 300/130/30/35", r.In, r.Out, r.Think, r.Cached)
	}
	if r.Requests != 2 {
		t.Errorf("请求数=%d want 2", r.Requests)
	}
}

// TestTokenUsageSeparatesDayAndModel 不同日期 / 不同模型 / 不同账号分别成行。
func TestTokenUsageSeparatesDayAndModel(t *testing.T) {
	s := NewTokenUsageStore("")
	d1 := time.Date(2026, 9, 11, 23, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 12, 1, 0, 0, 0, time.Local)

	s.Record("u1", "m1", d1, TokenDelta{In: 10, Out: 1})
	s.Record("u1", "m1", d2, TokenDelta{In: 20, Out: 2})
	s.Record("u1", "m2", d2, TokenDelta{In: 30, Out: 3})
	s.Record("u2", "m1", d2, TokenDelta{In: 40, Out: 4})

	if got := len(s.Rows()); got != 4 {
		t.Fatalf("应 4 行（账号/模型/日期各不同）, got %d", got)
	}
}

// TestTokenUsageRollups 账号级与全局汇总必须由明细 sum 得出。
func TestTokenUsageRollups(t *testing.T) {
	s := NewTokenUsageStore("")
	day := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)
	s.Record("u1", "m1", day, TokenDelta{In: 100, Out: 10, Think: 5, Cached: 60})
	s.Record("u1", "m2", day, TokenDelta{In: 200, Out: 20, Think: 5, Cached: 10})
	s.Record("u2", "m1", day, TokenDelta{In: 300, Out: 30, Think: 0, Cached: 0})

	// 账号级
	byAcct := s.ByAccount()
	if len(byAcct) != 2 {
		t.Fatalf("账号级应 2 行, got %d", len(byAcct))
	}
	var u1 *TokenUsageRow
	for i := range byAcct {
		if byAcct[i].UID == "u1" {
			u1 = &byAcct[i]
		}
	}
	if u1 == nil {
		t.Fatal("缺 u1")
	}
	if u1.In != 300 || u1.Out != 30 || u1.Think != 10 || u1.Cached != 70 {
		t.Errorf("u1 汇总错误: %+v", u1)
	}
	if u1.Requests != 2 {
		t.Errorf("u1 请求数=%d want 2", u1.Requests)
	}

	// 全局
	total := s.Total()
	if total.In != 600 || total.Out != 60 || total.Think != 10 || total.Cached != 70 {
		t.Errorf("全局汇总错误: %+v", total)
	}
	if total.Requests != 3 {
		t.Errorf("全局请求数=%d want 3", total.Requests)
	}
}

// TestTokenUsageMissingUsageCounted usage 缺失时也要计一次请求（否则成功率/覆盖度失真）。
func TestTokenUsageMissingUsageCounted(t *testing.T) {
	s := NewTokenUsageStore("")
	day := time.Now()
	s.Record("u1", "m1", day, TokenDelta{In: -1, Out: -1, Think: -1, Cached: -1})

	r := s.Rows()[0]
	if r.Requests != 1 {
		t.Errorf("请求数应计 1, got %d", r.Requests)
	}
	// -1（未知）不得污染总和：应钳为 0 计入
	if r.In != 0 || r.Out != 0 {
		t.Errorf("未知 token 应钳 0（不得为负）: In=%d Out=%d", r.In, r.Out)
	}
	// 但要记录"有多少次请求缺 usage"，供展示层提示
	if r.Missing != 1 {
		t.Errorf("Missing=%d want 1（缺 usage 计数）", r.Missing)
	}
}

// TestTokenUsageSortStable Rows 输出顺序稳定（按日期倒序、账号、模型），便于展示与测试。
func TestTokenUsageSortStable(t *testing.T) {
	s := NewTokenUsageStore("")
	d1 := time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 12, 0, 0, 0, 0, time.Local)
	s.Record("u2", "m1", d1, TokenDelta{In: 1, Out: 1})
	s.Record("u1", "m2", d2, TokenDelta{In: 1, Out: 1})
	s.Record("u1", "m1", d2, TokenDelta{In: 1, Out: 1})

	rows := s.Rows()
	if rows[0].Day != "2026-09-12" {
		t.Errorf("最新日期应在前, got %s", rows[0].Day)
	}
	if rows[0].UID != "u1" || rows[0].Model != "m1" {
		t.Errorf("同日应按账号+模型排序, got %s/%s", rows[0].UID, rows[0].Model)
	}
	if rows[2].Day != "2026-09-11" {
		t.Errorf("最旧日期应在后, got %s", rows[2].Day)
	}
}

// TestTokenUsageFilterByDay 按日期筛选（GUI 需要"只看今天"）。
func TestTokenUsageFilterByDay(t *testing.T) {
	s := NewTokenUsageStore("")
	d1 := time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 9, 12, 0, 0, 0, 0, time.Local)
	s.Record("u1", "m1", d1, TokenDelta{In: 100, Out: 1})
	s.Record("u1", "m1", d2, TokenDelta{In: 200, Out: 2})

	rows := s.RowsForDay("2026-09-12")
	if len(rows) != 1 || rows[0].In != 200 {
		t.Fatalf("按日筛选错误: %+v", rows)
	}
	if got := s.RowsForDay("1999-01-01"); len(got) != 0 {
		t.Errorf("无数据的日期应返回空, got %+v", got)
	}
}

// TestTokenUsagePersistence 落盘后重载仍在（重启不丢）。
func TestTokenUsagePersistence(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/token-usage.json"
	day := time.Date(2026, 9, 12, 10, 0, 0, 0, time.Local)

	s := NewTokenUsageStore(fp)
	s.Record("u1", "m1", day, TokenDelta{In: 100, Out: 50, Think: 20, Cached: 30})
	s.Close()

	s2 := NewTokenUsageStore(fp)
	defer s2.Close()
	rows := s2.Rows()
	if len(rows) != 1 {
		t.Fatalf("重载应 1 行, got %d", len(rows))
	}
	r := rows[0]
	if r.In != 100 || r.Out != 50 || r.Think != 20 || r.Cached != 30 || r.Requests != 1 {
		t.Errorf("重载内容错误: %+v", r)
	}
}

// TestTokenUsageCorruptFileSilent 文件损坏静默降级（不影响启动，与既有存储约定一致）。
func TestTokenUsageCorruptFileSilent(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/token-usage.json"
	if err := os.WriteFile(fp, []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewTokenUsageStore(fp)
	defer s.Close()
	if len(s.Rows()) != 0 {
		t.Error("损坏文件应降级为空")
	}
	s.Record("u1", "m1", time.Now(), TokenDelta{In: 1, Out: 1})
	if len(s.Rows()) != 1 {
		t.Error("降级后仍应能记录")
	}
}

// TestTokenUsageEmptyPath 空路径 = 纯内存（不写盘，功能正常）。
func TestTokenUsageEmptyPath(t *testing.T) {
	s := NewTokenUsageStore("")
	s.Record("u1", "m1", time.Now(), TokenDelta{In: 5, Out: 5})
	if len(s.Rows()) != 1 {
		t.Error("空路径下仍应记录到内存")
	}
}
