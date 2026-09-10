package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCheckinStorePersistsAcrossReload 验证核心修复：签到记录落盘后重载仍在。
// 这是"看不到签到记录"的主因——此前记录只存内存切片，进程重启即清空。
func TestCheckinStorePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "checkin-log.json")

	s := newCheckinStore(fp)
	s.Append(checkinRow{At: "09-11 09:00:00", UID: "u1", Result: "成功", Detail: ""})
	s.Append(checkinRow{At: "09-11 09:00:01", UID: "u2", Result: "今天已签到", Detail: ""})

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("记录未落盘: %v", err)
	}

	// 模拟进程重启
	s2 := newCheckinStore(fp)
	rows := s2.Rows()
	if len(rows) != 2 {
		t.Fatalf("重载后记录数=%d want 2", len(rows))
	}
	// 最新在前
	if rows[0].UID != "u2" || rows[1].UID != "u1" {
		t.Errorf("顺序错误: %+v", rows)
	}
	if rows[0].Result != "今天已签到" {
		t.Errorf("内容错误: %+v", rows[0])
	}
}

// TestCheckinStoreTodayChecked 验证启动补签去重逻辑：成功/已签到算已签，
// 失败不算（应允许重试），未签到不算。
func TestCheckinStoreTodayChecked(t *testing.T) {
	dir := t.TempDir()
	s := newCheckinStore(filepath.Join(dir, "checkin-log.json"))
	today := time.Now()

	// 失败记录不算已签（允许补签重试）
	s.Append(checkinRow{At: today.Format("01-02") + " 09:00:00", UID: "failed", Result: "失败"})
	if s.TodayChecked("failed", today) {
		t.Error("失败记录不应计为已签到（否则当天无法补签）")
	}

	s.Append(checkinRow{At: today.Format("01-02") + " 09:00:00", UID: "ok", Result: "成功"})
	if !s.TodayChecked("ok", today) {
		t.Error("成功记录应计为已签到")
	}

	s.Append(checkinRow{At: today.Format("01-02") + " 09:00:00", UID: "already", Result: "今天已签到"})
	if !s.TodayChecked("already", today) {
		t.Error("今天已签到应计为已签到")
	}

	if s.TodayChecked("never", today) {
		t.Error("无记录不应计为已签到")
	}

	// 昨天的记录不算今天
	y := today.AddDate(0, 0, -1)
	s.Append(checkinRow{At: y.Format("01-02") + " 09:00:00", UID: "yday", Result: "成功"})
	if s.TodayChecked("yday", today) {
		t.Error("昨天记录不应计为今天已签到")
	}
}

// TestCheckinStoreLimit 验证滚动窗口上限，防文件无限增长。
func TestCheckinStoreLimit(t *testing.T) {
	dir := t.TempDir()
	s := newCheckinStore(filepath.Join(dir, "checkin-log.json"))
	for i := 0; i < checkinLogLimit+50; i++ {
		s.Append(checkinRow{At: "09-11 09:00:00", UID: "u", Result: "成功"})
	}
	if got := len(s.Rows()); got != checkinLogLimit {
		t.Errorf("记录数=%d want %d", got, checkinLogLimit)
	}
}

// TestCheckinStoreNilPathFallback 验证不落盘时降级为纯内存，功能不中断。
func TestCheckinStoreNilPathFallback(t *testing.T) {
	s := newCheckinStore("")
	s.Append(checkinRow{At: "09-11 09:00:00", UID: "u1", Result: "成功"})
	if got := len(s.Rows()); got != 1 {
		t.Errorf("纯内存模式记录数=%d want 1", got)
	}
}

// TestCheckinStoreCorruptFileDegrades 验证文件损坏时静默降级，不影响启动。
func TestCheckinStoreCorruptFileDegrades(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "checkin-log.json")
	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCheckinStore(fp)
	if got := len(s.Rows()); got != 0 {
		t.Errorf("损坏文件应降级为空，got %d", got)
	}
	// 仍可正常追加
	s.Append(checkinRow{At: "09-11 09:00:00", UID: "u1", Result: "成功"})
	if got := len(s.Rows()); got != 1 {
		t.Errorf("损坏后追加失败，got %d", got)
	}
}
