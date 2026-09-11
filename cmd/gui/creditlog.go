// creditlog.go — 积分变动历史持久化（本地文件）。
//
// 目的：让"积分怎么变的"可见——签到 +100、chat 消耗、额度刷新带来的波动各留一条记录，
// 重启不丢；并且能按【单个账号】过滤出属于它的完整历史（GUI 双击账号直接看到）。
//
// 数据来源是 pool 的积分写路径回调（SetCreditsReason / ReenableIfCreditsReason），
// 只做观测：不参与选号、冷却、熔断任何决策，落盘失败也绝不回传错误影响主流程
// （与 checkinlog.go 同约定）。
//
// ── 存储格式：JSONL（每行一条记录，只追加）──
//
// 为什么不用 JSON 数组（checkinlog.go 的风格）：数组格式每次落盘都要【整体重写】，
// 而本项目的历史写入非常频繁——实测 auto-refresh 间隔可短到 2 分钟、3 个账号各写一条，
// 迟到 05:19 的 3 条记录把 05:11~05:13 的旧记录挤出了显示窗口，用户看到的"变化历史"
// 便不再完整。JSONL 只 append 一行，代价与历史长度无关，也不会重写既有记录，
// 因此可以在同样成本下把保留上限从 2000 提到 20000。
//
// 兼容性：读取时同时接受【旧的 JSON 数组】格式（首字符 '['）与新的 JSONL 格式，
// 因此升级不会丢历史；写回一律用 JSONL。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"workbuddy2api/internal/pool"
)

const (
	creditLogFile = "credit-log.jsonl"
	// creditLogLimit 保留条数上限（JSONL 只追加，可承受更大的窗口）。
	creditLogLimit = 20000

	// creditLogLegacyFile 旧版数组格式文件名。启动时若新文件不存在而它存在，则导入并保留原文件。
	creditLogLegacyFile = "credit-log.json"

	// tokenUsageFile token 用量统计落盘文件名（账号×模型×日期，见 server/tokenusage.go）。
	tokenUsageFile = "token-usage.json"
)

// creditRow 一条积分变动记录。
//
// Old/New 是积分前后快照，Delta = New - Old。落盘存原始数值（便于日后重新格式化），
// 展示文案在界面上生成。
type creditRow struct {
	At     string `json:"at"`            // "01-02 15:04:05"（与签到记录同格式）
	UID    string `json:"uid,omitempty"` // 落盘存 UID（昵称会变），显示时解析
	Old    int64  `json:"old"`
	New    int64  `json:"new"`
	Delta  int64  `json:"delta"`
	First  bool   `json:"first,omitempty"`  // 首次拿到余额，delta 无参照系
	Reason string `json:"reason,omitempty"` // 变动来源
}

// creditStore 积分历史存储：线程安全 + 追加落盘 + 内存滚动窗口。
type creditStore struct {
	mu   sync.Mutex
	path string
	rows []creditRow // 最新在前
	// file 常开的追加句柄（nil = 尚未打开或不可写）。
	// 保持常开是为了让每次变动只做一次 write 系统调用，避免重复 open/close。
	file *os.File
	// opened 标记已尝试过打开（无论成败），避免写盘持续失败时每条记录都重试 open。
	opened bool
}

// newCreditStore 构建存储；path 为空表示不落盘（降级纯内存，功能不中断）。
func newCreditStore(path string) *creditStore {
	s := &creditStore{path: path}
	s.load()
	return s
}

// load 启动时读取历史；文件缺失/损坏静默降级为空（不影响启动）。
// 同时兼容旧的 JSON 数组格式。
func (s *creditStore) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		// 新文件不在：尝试导入旧数组格式的历史（升级不丢记录）。
		raw = s.loadLegacy()
		if raw == nil {
			return
		}
	}
	rows := parseCreditLog(raw)
	if len(rows) > creditLogLimit {
		rows = rows[:creditLogLimit]
	}
	s.rows = rows
}

// loadLegacy 读取旧版 credit-log.json（数组格式）。不存在/不可读返回 nil。
// 只读不删：旧文件保留原地，便于回退与人工核对。
func (s *creditStore) loadLegacy() []byte {
	legacy := filepath.Join(filepath.Dir(s.path), creditLogLegacyFile)
	if legacy == s.path {
		return nil
	}
	raw, err := os.ReadFile(legacy)
	if err != nil {
		return nil
	}
	return raw
}

// parseCreditLog 解析落盘内容，兼容两种格式，返回【最新在前】的记录。
//
//   - JSONL（首字符不是 '['）：文件按写入顺序（旧→新）追加，故解析后需反转。
//   - JSON 数组（首字符 '['）：历史实现按"最新在前"整体重写，故直接用。
//
// 逐行解析时单行损坏只跳过该行（不整份丢弃），使一次半截写入不会抹掉全部历史。
func parseCreditLog(raw []byte) []creditRow {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '[' {
		var rows []creditRow
		if json.Unmarshal(trimmed, &rows) != nil {
			return nil
		}
		return rows
	}
	var rows []creditRow
	sc := bufio.NewScanner(bytes.NewReader(trimmed))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r creditRow
		if json.Unmarshal(line, &r) != nil {
			continue // 损坏行跳过，保留其余历史
		}
		rows = append(rows, r)
	}
	// 文件为旧→新，界面/内存约定最新在前 → 反转。
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

// Rows 返回记录副本（最新在前）。
func (s *creditStore) Rows() []creditRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]creditRow, len(s.rows))
	copy(out, s.rows)
	return out
}

// creditAllUIDs 是"全部账号"的哨兵值，供过滤下拉框使用。
// 过滤在界面模型层完成（历史已全量在内存），无需回读磁盘。
const creditAllUIDs = "*"

// AppendChange 把一次 pool 积分变动转为记录并落盘。
// 由 pool 回调调用，可能来自任意 goroutine，因此内部自行加锁。
func (s *creditStore) AppendChange(ch pool.CreditChange) {
	s.Append(creditRow{
		At:     ch.At.Format("01-02 15:04:05"),
		UID:    ch.UID,
		Old:    ch.Old,
		New:    ch.New,
		Delta:  ch.Delta,
		First:  ch.First,
		Reason: ch.Reason,
	})
}

// Append 插入一条记录（内存最新在前）并追加落盘。
func (s *creditStore) Append(r creditRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append([]creditRow{r}, s.rows...)
	if len(s.rows) > creditLogLimit {
		s.rows = s.rows[:creditLogLimit]
	}
	s.appendLocked(r)
}

// appendLocked 追加一行 JSONL 到磁盘。调用方需已持锁。
// 失败仅忽略——积分历史属观测数据，绝不能因写盘失败影响签到/刷新主流程。
func (s *creditStore) appendLocked(r creditRow) {
	f := s.openLocked()
	if f == nil {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		// 句柄失效（磁盘满/被删/句柄被回收）：丢弃句柄并允许下次重开。
		// 不重试本次写入——历史记录丢一条可接受，阻塞主流程不可接受。
		_ = f.Close()
		s.file = nil
		s.opened = false
	}
}

// openLocked 返回可写的追加句柄；不可用时返回 nil。调用方需已持锁。
//
// 目录创建失败或文件不可写时只尝试一次（opened 标记），之后静默降级，
// 避免每条记录都重复一次注定失败的系统调用。
func (s *creditStore) openLocked() *os.File {
	if s.file != nil {
		return s.file
	}
	if s.opened || s.path == "" {
		return nil
	}
	s.opened = true
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil
		}
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	s.file = f
	return f
}

// Close 关闭落盘句柄（退出/测试清理用）。幂等。
func (s *creditStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// creditDeltaText 把一次变动格式化成表格用的"变动"列文案。
// 首次获取不谎报涨跌（显示"首次获取"），其余带符号显示。
func creditDeltaText(r creditRow) string {
	if r.First {
		return "首次获取"
	}
	switch {
	case r.Delta > 0:
		return "+" + itoa(r.Delta)
	case r.Delta < 0:
		return itoa(r.Delta)
	default:
		return "无变化"
	}
}

// creditRangeText 把前后值渲染为"旧 → 新"。
func creditRangeText(r creditRow) string {
	return itoa(r.Old) + " → " + itoa(r.New)
}

// creditMatchUID 报告记录是否属于该账号（供界面过滤）；uid 为"全部"时恒真。
func creditMatchUID(r creditRow, uid string) bool {
	if uid == "" || uid == creditAllUIDs {
		return true
	}
	return r.UID == uid
}

// itoa 避免为单处格式化引入 strconv（保持与文件其余部分一致的极简依赖）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
