package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// newTestCreditStore 建一个落在临时目录的存储，并在测试结束关闭句柄。
// 必须 Close：creditStore 为减少系统调用常开追加句柄，不关会让 t.TempDir 的
// RemoveAll 在 Windows 上因文件被占用而失败（unlinkat: used by another process）。
func newTestCreditStore(t *testing.T, name string) (*creditStore, string) {
	t.Helper()
	fp := filepath.Join(t.TempDir(), name)
	s := newCreditStore(fp)
	t.Cleanup(s.Close)
	return s, fp
}

// TestCreditStorePersistsAcrossReload 核心：积分记录落盘后重载仍在（重启不丢）。
func TestCreditStorePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, creditLogFile)

	s := newCreditStore(fp)
	s.AppendChange(pool.CreditChange{
		UID: "u1", Old: 930, New: 1030, Delta: 100, Reason: "签到", At: time.Now(),
	})
	s.AppendChange(pool.CreditChange{
		UID: "u2", Old: 500, New: 480, Delta: -20, Reason: "自动刷新", At: time.Now(),
	})
	s.Close() // 落盘改用常开句柄，重载前先关闭以模拟进程退出

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("积分记录未落盘: %v", err)
	}

	// 模拟进程重启
	s2 := newCreditStore(fp)
	defer s2.Close()
	rows := s2.Rows()
	if len(rows) != 2 {
		t.Fatalf("重载条数=%d want 2", len(rows))
	}
	// 最新在前
	if rows[0].UID != "u2" || rows[1].UID != "u1" {
		t.Errorf("顺序应最新在前: %+v", rows)
	}
	if rows[1].Delta != 100 || rows[1].Reason != "签到" {
		t.Errorf("签到记录内容丢失: %+v", rows[1])
	}
}

// TestCreditStoreLimit 上限截断：超过 creditLogLimit 只保留最新。
// 直接构造超限切片后经 Append 单次触发截断，避免 20000+ 次落盘拖慢测试。
func TestCreditStoreLimit(t *testing.T) {
	s, _ := newTestCreditStore(t, creditLogFile)
	full := make([]creditRow, creditLogLimit)
	for i := range full {
		full[i] = creditRow{UID: "u", Old: int64(i)}
	}
	s.mu.Lock()
	s.rows = full
	s.mu.Unlock()
	s.AppendChange(pool.CreditChange{UID: "newest", Old: 1, New: 2, Delta: 1, At: time.Now()})
	rows := s.Rows()
	if len(rows) != creditLogLimit {
		t.Errorf("记录数=%d want %d", len(rows), creditLogLimit)
	}
	if rows[0].UID != "newest" {
		t.Errorf("截断后应保留最新记录: %+v", rows[0])
	}
}

// TestCreditStoreMissingFileSilent 文件缺失静默降级（不影响启动）。
func TestCreditStoreMissingFileSilent(t *testing.T) {
	s, _ := newTestCreditStore(t, "nope.jsonl")
	if len(s.Rows()) != 0 {
		t.Error("缺失文件应得到空历史")
	}
	s.AppendChange(pool.CreditChange{UID: "u1", Old: 0, New: 5, Delta: 5, At: time.Now()})
	if len(s.Rows()) != 1 {
		t.Error("append 后应有 1 条")
	}
}

// TestCreditStoreCorruptFileSilent 文件损坏静默降级，不 panic。
func TestCreditStoreCorruptFileSilent(t *testing.T) {
	s, fp := newTestCreditStore(t, creditLogFile)
	if err := os.WriteFile(fp, []byte("not json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.load()
	if len(s.Rows()) != 0 {
		t.Error("损坏文件应降级为空")
	}
}

// TestCreditStorePartialCorruptionKeepsGoodLines JSONL 单行损坏只丢那一行，
// 其余历史必须保留（JSON 数组格式下一处损坏会丢全部，这是换格式的收益之一）。
func TestCreditStorePartialCorruptionKeepsGoodLines(t *testing.T) {
	s, fp := newTestCreditStore(t, creditLogFile)
	body := `{"at":"09-11 10:00:00","uid":"u1","delta":100}
{"at":"broken
{"at":"09-11 11:00:00","uid":"u2","delta":-5}
`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s.load()
	rows := s.Rows()
	if len(rows) != 2 {
		t.Fatalf("有效行数=%d want 2（损坏行应被跳过而非整份丢弃）: %+v", len(rows), rows)
	}
	// 文件是旧→新，重载后最新在前。
	if rows[0].UID != "u2" || rows[1].UID != "u1" {
		t.Errorf("顺序应最新在前: %+v", rows)
	}
}

// TestCreditStoreEmptyPathNoDisk 空路径 = 纯内存模式，不写盘且功能正常。
func TestCreditStoreEmptyPathNoDisk(t *testing.T) {
	s := newCreditStore("")
	s.AppendChange(pool.CreditChange{UID: "u1", Old: 1, New: 2, Delta: 1, At: time.Now()})
	if len(s.Rows()) != 1 {
		t.Error("空路径下仍应记录到内存")
	}
}

// TestCreditStoreAppendOnlyGrows JSONL 格式语义：多次写入是追加，文件行数随记录增长，
// 且不会重写既有行（旧记录的字节保持不变）。
func TestCreditStoreAppendOnlyGrows(t *testing.T) {
	s, fp := newTestCreditStore(t, creditLogFile)
	s.AppendChange(pool.CreditChange{UID: "u1", Old: 0, New: 100, Delta: 100, Reason: "签到", At: time.Now()})
	raw1, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw1), "\n"); n != 1 {
		t.Fatalf("首条后行数=%d want 1", n)
	}
	s.AppendChange(pool.CreditChange{UID: "u2", Old: 0, New: 50, Delta: 50, Reason: "自动刷新", At: time.Now()})
	raw2, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw2), "\n"); n != 2 {
		t.Fatalf("第二条后行数=%d want 2", n)
	}
	// 追加语义：先写入的字节仍是新文件的前缀（未被重写）。
	if !strings.HasPrefix(string(raw2), string(raw1)) {
		t.Errorf("追加写入不应重写既有行:\n first=%q\nsecond prefix=%q", raw1, string(raw2[:len(raw1)]))
	}
}

// TestCreditStoreImportsLegacyArrayFormat 升级兼容：旧 credit-log.json（JSON 数组）
// 在新文件不存在时被导入，历史不丢；且旧格式“最新在前”的顺序被正确保留。
func TestCreditStoreImportsLegacyArrayFormat(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, creditLogLegacyFile)
	body := `[
  {"at":"09-11 11:00:00","uid":"u2","old":500,"new":480,"delta":-20,"reason":"自动刷新"},
  {"at":"09-11 10:00:00","uid":"u1","old":930,"new":1030,"delta":100,"reason":"签到"}
]`
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCreditStore(filepath.Join(dir, creditLogFile))
	defer s.Close()
	rows := s.Rows()
	if len(rows) != 2 {
		t.Fatalf("导入条数=%d want 2", len(rows))
	}
	if rows[0].UID != "u2" || rows[1].UID != "u1" {
		t.Errorf("旧数组“最新在前”顺序应保留: %+v", rows)
	}
	if rows[1].Reason != "签到" {
		t.Errorf("旧记录内容丢失: %+v", rows[1])
	}
	// 旧文件保留原地（只读导入，便于回退与人工核对）。
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("旧文件不应被删除或改写: %v", err)
	}
}

// TestCreditModelFilterView 表格模型过滤：RowCount/Value 只反映被选中的账号，
// 且底层完整历史不被破坏（切换回“全部”仍能看到全部记录）。
func TestCreditModelFilterView(t *testing.T) {
	m := &creditModel{display: func(uid string) string { return "名-" + uid }}
	m.Replace([]creditRow{
		{At: "09-11 12:00:00", UID: "u1", Old: 200, New: 190, Delta: -10, Reason: "自动刷新"},
		{At: "09-11 11:00:00", UID: "u2", Old: 500, New: 480, Delta: -20, Reason: "自动刷新"},
		{At: "09-11 10:00:00", UID: "u1", Old: 100, New: 200, Delta: 100, Reason: "签到"},
	})

	if m.RowCount() != 3 {
		t.Fatalf("全部视图行数=%d want 3", m.RowCount())
	}

	m.SetFilter("u1")
	if m.RowCount() != 2 {
		t.Fatalf("u1 视图行数=%d want 2", m.RowCount())
	}
	// 账号列显示昵称，但过滤按原始 UID 生效。
	if got := m.Value(0, 1); got != "名-u1" {
		t.Errorf("账号列=%v want 名-u1（显示昵称）", got)
	}
	if got := m.Value(1, 2); got != "+100" {
		t.Errorf("u1 第二行变动=%v want +100", got)
	}
	if got := m.Value(0, 3); got != "200 → 190" {
		t.Errorf("积分变化列=%v want \"200 → 190\"", got)
	}
	// 越界安全。
	if got := m.Value(5, 0); got != "" {
		t.Errorf("越界取值应返回空串，got %v", got)
	}

	// 切回全部：完整历史仍在（过滤只是视图层，不裁剪数据）。
	m.SetFilter(creditAllUIDs)
	if m.RowCount() != 3 {
		t.Errorf("切回全部行数=%d want 3", m.RowCount())
	}
}

// TestCreditModelCurrentFollowsFilter Current() 必须与 RowCount/Value 同口径，
// 否则双击/选中的行会与被过滤后的显示错位。
func TestCreditModelCurrentFollowsFilter(t *testing.T) {
	m := &creditModel{}
	m.Replace([]creditRow{
		{UID: "u1", Delta: 1},
		{UID: "u2", Delta: 2},
		{UID: "u1", Delta: 3},
	})
	if r, ok := m.Current(0); !ok || r.UID != "u1" {
		t.Fatalf("全部视图第 0 行=%+v ok=%v", r, ok)
	}
	m.SetFilter("u2")
	if r, ok := m.Current(0); !ok || r.UID != "u2" || r.Delta != 2 {
		t.Errorf("u2 视图第 0 行=%+v ok=%v want u2/delta=2", r, ok)
	}
	if _, ok := m.Current(1); ok {
		t.Error("u2 视图只有 1 行，第 1 行应不存在")
	}
}

// TestCreditDeltaText 变动文案：正数带 +、负数带 -、首次不谎报、零为无变化。
func TestCreditDeltaText(t *testing.T) {
	cases := []struct {
		row  creditRow
		want string
	}{
		{creditRow{Delta: 100}, "+100"},
		{creditRow{Delta: -50}, "-50"},
		{creditRow{Delta: 0}, "无变化"},
		{creditRow{Delta: 500, First: true}, "首次获取"},
	}
	for _, c := range cases {
		if got := creditDeltaText(c.row); got != c.want {
			t.Errorf("creditDeltaText(%+v)=%q want %q", c.row, got, c.want)
		}
	}
}

// TestCreditModelRendersRows 表格模型列映射：时间/账号/变动/积分变化/来源。
func TestCreditModelRendersRows(t *testing.T) {
	m := &creditModel{}
	m.Prepend(creditRow{At: "09-11 09:00:00", UID: "张三", Old: 930, New: 1030, Delta: 100, Reason: "签到"})
	if m.RowCount() != 1 {
		t.Fatalf("行数=%d", m.RowCount())
	}
	want := []string{"09-11 09:00:00", "张三", "+100", "930 → 1030", "签到"}
	for col, w := range want {
		if got := m.Value(0, col); got != w {
			t.Errorf("col %d=%v want %q", col, got, w)
		}
	}
}
