// rotatelog.go — gui.log 的按天 + 按大小轮转。
//
// 背景：gui.log 以 O_APPEND 无限追加，此前没有任何轮转机制。按当前日志量
// （实测约 1~2 MB/天）一年会积累数百 MB，且排障时要在几个 GB 的单文件里 grep。
//
// 规则（刻意保持简单，不引第三方依赖）：
//   - 按天切：写入时发现日期变了 → 把当前 gui.log 改名为 gui.log.YYYY-MM-DD 后新建；
//   - 按大小切：单文件超过 maxBytes 也立即切（防止单日日志暴涨，如重复报错刷屏）；
//   - 保留 keep 期限：轮转时删除过期的 gui.log.YYYY-MM-DD 归档；
//     只删除【严格匹配自身日期命名规则】的文件，绝不碰其他文件。
//
// 线程安全：标准日志与请求表格日志可能来自任意 goroutine，写路径全程持锁。
// 锁内做的是 append + 偶尔的改名/删除，量级可接受（与 creditStore 的既有取舍一致）。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// guiLogMaxBytes 单文件大小上限：超过即切（5MB 约等于数万行请求日志）。
	guiLogMaxBytes = 5 << 20
	// guiLogKeepDays 归档保留天数：超过后删除。
	guiLogKeepDays = 14
)

// rotatingLogWriter 一个会按天/按大小轮转的追加写文件。
type rotatingLogWriter struct {
	mu       sync.Mutex
	path     string // 当前活动文件（gui.log）
	maxBytes int64
	keep     time.Duration
	f        *os.File
	curDay   string // 当前文件对应的日期（YYYY-MM-DD）
	curSize  int64
	bytes    int64 // 已写入总量（测试/观测用）
	rotates  int   // 已发生轮转次数（测试/观测用）
	// now 可注入替换以便测试跨天场景（nil = time.Now）。
	now func() time.Time
}

// newRotatingLogWriter 打开（或创建）日志文件。打开失败不 panic：后续 Write 会再试。
func newRotatingLogWriter(path string, maxBytes int64, keep time.Duration) *rotatingLogWriter {
	return &rotatingLogWriter{
		path:     path,
		maxBytes: maxBytes,
		keep:     keep,
	}
}

// ensureForSetup 启动装配阶段立即打开文件：成功则确认该候选路径可用，
// 失败则让调用方换下一个候选目录。这与 lazy 的 Write 路径互不影响。
func (r *rotatingLogWriter) ensureForSetup() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureLocked()
}

// Write 实现 io.Writer。内部自动处理跨天/超限切换与过期清理。
func (r *rotatingLogWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureLocked(); err != nil {
		return 0, err
	}
	// 跨天 或 超限 → 轮转。
	now := r.clock()
	day := now.Format("2006-01-02")
	if day != r.curDay || (r.maxBytes > 0 && r.curSize+int64(len(p)) > r.maxBytes) {
		if err := r.rotateLocked(now, day); err != nil {
			// 轮转失败（如磁盘满/权限问题）则继续往旧文件写，绝不让日志丢失。
			// 下次写入会再次尝试轮转。
		}
	}
	n, err := r.f.Write(p)
	if n > 0 {
		r.curSize += int64(n)
		r.bytes += int64(n)
	}
	return n, err
}

// ensureLocked 确保文件已打开（首次写或 Close 后重写）。
func (r *rotatingLogWriter) ensureLocked() error {
	if r.f != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f = f
	r.curSize = st.Size()
	r.curDay = r.clock().Format("2006-01-02")
	// 启动时清一次过期归档（进程可能数周不重启）。
	r.cleanupLocked(r.clock())
	return nil
}

// rotateLocked 把当前文件改名归档并打开新文件。
// 归档名 gui.log.YYYY-MM-DD；若当天已存在归档则加时间后缀，避免覆盖。
func (r *rotatingLogWriter) rotateLocked(now time.Time, day string) error {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	if st, err := os.Stat(r.path); err == nil && st.Size() > 0 {
		// 归档名必须用【被轮转文件的日期】（轮转前的 curDay，即数据所属的那天），
		// 而不是新日期——否则 9 月 10 日的内容会被归档成 gui.log.2026-09-11，
		// 按归档名清理过期文件时就会把晚一天的文件算错账。
		archDay := r.curDay
		if archDay == "" || archDay == day {
			// 无记录（极端：进程内第一次写就触发大小轮转）→ 用当前日期兜底。
			archDay = day
		}
		arch := r.archivedName(now, archDay)
		if err := os.Rename(r.path, arch); err != nil {
			// 改名失败（被占用/权限）→ 保持现状继续写旧文件。
			f, oerr := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if oerr != nil {
				return oerr
			}
			r.f = f
			return err
		}
		r.rotates++
	} else {
		// 空文件或不存在：直接删掉再新建，避免留空壳。
		_ = os.Remove(r.path)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	r.f = f
	r.curSize = 0
	r.curDay = day
	r.cleanupLocked(now)
	return nil
}

// archivedName 归档文件名。同一天第二次轮转（按大小）时加序号后缀避免互相覆盖。
func (r *rotatingLogWriter) archivedName(now time.Time, day string) string {
	base := r.path + "." + day
	if _, err := os.Stat(base); err == nil {
		// 当天已有归档（大概率是按大小切的第二份）→ 加时刻后缀。
		return fmt.Sprintf("%s.%s", base, now.Format("150405"))
	}
	return base
}

// cleanupLocked 删除过期的归档。只删严格匹配「gui.log.YYYY-MM-DD」或
// 「gui.log.YYYY-MM-DD.HHMMSS」命名规则的文件——其他文件一律不碰。
func (r *rotatingLogWriter) cleanupLocked(now time.Time) {
	dir := filepath.Dir(r.path)
	base := filepath.Base(r.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-r.keep)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+".") {
			continue
		}
		suffix := strings.TrimPrefix(name, base+".")
		// 日期部分：要么整段是日期，要么是「日期.时刻」。
		dayPart := suffix
		if i := strings.Index(suffix, "."); i > 0 {
			dayPart = suffix[:i]
		}
		t, err := time.ParseInLocation("2006-01-02", dayPart, time.Local)
		if err != nil {
			continue // 非日期命名（如 gui.log.custom-backup）→ 不是我们的归档，跳过
		}
		// 以归档当天作为「最后写入日」近似判断过期。
		if t.Before(cutoff.Truncate(24 * time.Hour)) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// clock 返回当前时刻（测试可注入）。
func (r *rotatingLogWriter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Close 关闭当前句柄。幂等；Close 后再 Write 会重新打开（等价于进程重启的语义）。
func (r *rotatingLogWriter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	r.curSize = 0
	return err
}
