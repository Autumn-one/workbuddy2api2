package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rotatingLogWriter 的行为测试。核心关注四点：
//  1. 按天切换（跨天时改名旧文件并新建）
//  2. 按大小切换（超过上限立即切，不等到跨天）
//  3. 只删除【自己命名规则】的过期文件（绝不误删别的文件）
//  4. 写入是线程安全的且数据完整（日志可能来自多个 goroutine）

func newTestRotator(t *testing.T, dir string, maxBytes int64, keep time.Duration) *rotatingLogWriter {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	return newRotatingLogWriter(filepath.Join(dir, "gui.log"), maxBytes, keep)
}

func TestRotatorWritesAndCreatesFile(t *testing.T) {
	w := newTestRotator(t, "", 1<<20, 14*24*time.Hour)
	defer w.Close()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(w.path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "hello\n" {
		t.Errorf("内容=%q want %q", raw, "hello\n")
	}
}

// TestRotatorRotatesOnDayBoundary 跨天写入 → 旧文件改名归档，新文件只含新行。
func TestRotatorRotatesOnDayBoundary(t *testing.T) {
	dir := t.TempDir()
	w := newTestRotator(t, dir, 1<<20, 14*24*time.Hour)
	defer w.Close()

	w.now = func() time.Time { return time.Date(2026, 9, 10, 23, 59, 0, 0, time.Local) }
	if _, err := w.Write([]byte("day1\n")); err != nil {
		t.Fatal(err)
	}
	// 模拟跨天
	w.now = func() time.Time { return time.Date(2026, 9, 11, 0, 0, 30, 0, time.Local) }
	if _, err := w.Write([]byte("day2\n")); err != nil {
		t.Fatal(err)
	}

	// 归档文件存在且含 day1
	arch := filepath.Join(dir, "gui.log.2026-09-10")
	raw, err := os.ReadFile(arch)
	if err != nil {
		t.Fatalf("归档文件不存在: %v", err)
	}
	if !strings.Contains(string(raw), "day1") {
		t.Errorf("归档应含 day1: %q", raw)
	}
	if strings.Contains(string(raw), "day2") {
		t.Errorf("归档不应含跨天后的内容: %q", raw)
	}
	// 当前文件只含 day2
	cur, _ := os.ReadFile(w.path)
	if strings.Contains(string(cur), "day1") {
		t.Errorf("当前文件不应含跨天前内容: %q", cur)
	}
	if !strings.Contains(string(cur), "day2") {
		t.Errorf("当前文件应含 day2: %q", cur)
	}
}

// TestRotatorRotatesOnSize 单文件超上限立即切，不依赖跨天。
func TestRotatorRotatesOnSize(t *testing.T) {
	dir := t.TempDir()
	w := newTestRotator(t, dir, 100, 14*24*time.Hour) // 100 字节就切
	defer w.Close()

	if _, err := w.Write([]byte(strings.Repeat("x", 90) + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("y", 90) + "\n")); err != nil {
		t.Fatal(err)
	}
	// 第二次写触发了切换 → 当前文件只含第二段
	cur, _ := os.ReadFile(w.path)
	if strings.Contains(string(cur), "xxxx") {
		t.Errorf("超过上限后当前文件不应还留着旧数据: %q", cur)
	}
	if !strings.Contains(string(cur), "yyyy") {
		t.Errorf("当前文件应含新写入: %q", cur)
	}
	// 归档存在
	entries, _ := filepath.Glob(filepath.Join(dir, "gui.log.*"))
	if len(entries) == 0 {
		t.Error("应产生归档文件")
	}
}

// TestRotatorDeletesOnlyOwnExpiredFiles 只删匹配 gui.log.* 命名规则且过期的，
// 同目录里其他文件（哪怕名字像）一律不动。
func TestRotatorDeletesOnlyOwnExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	keep := 24 * time.Hour
	old := time.Now().Add(-48 * time.Hour)

	w := newTestRotator(t, dir, 1<<20, keep)
	defer w.Close()

	// 过期的归档（应被删）
	staleArchive := filepath.Join(dir, "gui.log."+old.Format("2006-01-02"))
	os.WriteFile(staleArchive, []byte("stale"), 0o600)
	// 未过期的归档（应保留）
	freshArchive := filepath.Join(dir, "gui.log."+time.Now().Format("2006-01-02"))
	os.WriteFile(freshArchive, []byte("fresh"), 0o600)
	// 名字相似但不是日志归档的文件（绝不能删）
	precious := filepath.Join(dir, "gui.log.custom-backup")
	os.WriteFile(precious, []byte("important"), 0o600)
	// 无关文件
	other := filepath.Join(dir, "state.json")
	os.WriteFile(other, []byte("{}"), 0o600)

	w.now = func() time.Time { return time.Now() }
	w.Write([]byte("trigger\n")) // 触发一次清理

	if _, err := os.Stat(staleArchive); !os.IsNotExist(err) {
		t.Errorf("过期归档应被删除")
	}
	if _, err := os.Stat(freshArchive); err != nil {
		t.Errorf("未过期归档不应被删除: %v", err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Errorf("非日期命名的文件绝不能删: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("无关文件不应被删除: %v", err)
	}
}

// TestRotatorConcurrentWrites 并发写不 panic、数据不交错。
func TestRotatorConcurrentWrites(t *testing.T) {
	w := newTestRotator(t, "", 1<<20, 14*24*time.Hour)
	defer w.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				w.Write([]byte("goroutine-line\n"))
			}
		}(i)
	}
	wg.Wait()
	raw, _ := os.ReadFile(w.path)
	lines := strings.Count(string(raw), "goroutine-line\n")
	if lines != 400 {
		t.Errorf("并发写应完整落盘 400 行，实际 %d", lines)
	}
}

// TestRotatorCloseIsIdempotent 重复 Close 不 panic（退出路径可能调用多次）。
func TestRotatorCloseIsIdempotent(t *testing.T) {
	w := newTestRotator(t, "", 1<<20, 14*24*time.Hour)
	w.Write([]byte("x\n"))
	w.Close()
	w.Close() // 第二次不应 panic
}

// TestRotatorReopenAfterClose 与退出-重启语义一致：Close 后再写应能重新打开。
func TestRotatorReopenAfterClose(t *testing.T) {
	w := newTestRotator(t, "", 1<<20, 14*24*time.Hour)
	defer w.Close()
	w.Write([]byte("before\n"))
	w.Close()
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatalf("Close 后再写应可恢复: %v", err)
	}
	raw, _ := os.ReadFile(w.path)
	if !strings.Contains(string(raw), "after\n") {
		t.Errorf("重开写入失败: %q", raw)
	}
}

// TestRotatorSizeRotationProducesArchives 与 setupLogging 相同用法下验证
// 「按大小轮转产生归档、当前文件保持在阈值内」——模拟单日日志暴涨的场景。
func TestRotatorSizeRotationProducesArchives(t *testing.T) {
	dir := t.TempDir()
	rw := newRotatingLogWriter(filepath.Join(dir, "gui.log"), 200, 14*24*time.Hour)
	defer rw.Close()

	// 连续写 3 轮，每轮 150 字节：第 2、3 次写入会触发超限切换
	for i := 0; i < 3; i++ {
		if _, err := rw.Write([]byte(strings.Repeat(string(rune('a'+i)), 149) + "\n")); err != nil {
			t.Fatal(err)
		}
	}

	entries, _ := filepath.Glob(filepath.Join(dir, "gui.log*"))
	if len(entries) != 3 {
		t.Fatalf("应有 3 个文件（当前 + 2 份归档），实际 %d: %v", len(entries), entries)
	}
	// 当前文件必须低于阈值（否则说明切换没生效，会无限增长）
	cur, _ := os.ReadFile(rw.path)
	if int64(len(cur)) > 200 {
		t.Errorf("当前文件 %d 字节超过阈值 200", len(cur))
	}
	// 同一天的归档必须带时刻后缀，避免按大小切时互相覆盖
	archived := 0
	for _, e := range entries {
		if e == rw.path {
			continue
		}
		archived++
		if !strings.Contains(filepath.Base(e), "gui.log.2026-") {
			t.Errorf("归档命名异常: %s", e)
		}
	}
	if archived != 2 {
		t.Errorf("归档数=%d want 2", archived)
	}
}
