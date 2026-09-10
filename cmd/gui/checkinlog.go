// checkinlog.go — 签到记录持久化。
//
// 背景（缺陷修复）：签到记录此前只存在 checkinModel.items 内存切片里，
// 进程重启即清空；而定时签到只在 09:00/21:00 整点触发，导致用户"看不到签到记录"。
//
// 本文件提供：
//  1. 记录落盘（data/checkin-log.json，原子写 tmp+rename，0600）——重启不丢
//  2. 每日去重查询（TodayChecked）——供启动补签判断，保证每账号每天最多签一次
//
// 存储格式刻意保持简单（扁平数组，最新在前），与项目其它 JSON 落盘风格一致。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	checkinLogFile  = "checkin-log.json"
	checkinLogLimit = 500 // 落盘上限（内存表格显示 300，落盘多留一些）
)

// checkinStore 签到记录存储：线程安全 + 原子落盘。
type checkinStore struct {
	mu   sync.Mutex
	path string
	rows []checkinRow
}

// newCheckinStore 构建存储；path 为空表示不落盘（降级为纯内存，功能不中断）。
func newCheckinStore(path string) *checkinStore {
	s := &checkinStore{path: path}
	s.load()
	return s
}

// load 启动时读取历史记录；文件缺失/损坏静默降级为空（不影响启动）。
func (s *checkinStore) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var rows []checkinRow
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	if len(rows) > checkinLogLimit {
		rows = rows[:checkinLogLimit]
	}
	s.rows = rows
}

// Rows 返回记录副本（最新在前）。
func (s *checkinStore) Rows() []checkinRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]checkinRow, len(s.rows))
	copy(out, s.rows)
	return out
}

// Append 插入一条记录（最新在前）并落盘。
func (s *checkinStore) Append(r checkinRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append([]checkinRow{r}, s.rows...)
	if len(s.rows) > checkinLogLimit {
		s.rows = s.rows[:checkinLogLimit]
	}
	s.saveLocked()
}

// saveLocked 原子落盘。调用方需已持锁。失败仅忽略——签到记录属观测数据，
// 绝不能因写盘失败影响签到主流程。
func (s *checkinStore) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.rows, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// TodayChecked 报告该 uid 今天是否已有【成功或已签到】记录。
// 供启动补签去重：失败记录不算（失败应允许重试）。
// day 用本地时区比较日期部分。
func (s *checkinStore) TodayChecked(uid string, day time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// At 格式 "01-02 15:04:05" 不含年份——按 MM-DD 比较；
	// 跨年边界由 checkinLogLimit=500 的滚动窗口自然覆盖（记录量远小于一年）。
	want := day.Format("01-02")
	for _, r := range s.rows {
		if r.UID != uid {
			continue
		}
		if len(r.At) >= 5 && r.At[:5] == want {
			if r.Result == "成功" || r.Result == "今天已签到" {
				return true
			}
		}
	}
	return false
}
