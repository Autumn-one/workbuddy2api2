// tokenusage.go — Token 用量统计（账号 × 模型 × 日期）。
//
// 目的：回答"token 花在哪了"——哪个账号、哪个模型、哪一天用了多少 token。
// 数据来源是上游响应里原样透传的 usage（prompt_tokens / completion_tokens /
// reasoning_tokens / cached_tokens），只在【成功的请求】里存在：
//   - 失败请求（6004 限流、402 余额不足等）上游不返回 usage，故本统计只覆盖成功流量；
//   - 因此它【不能用于核对积分扣减】（积分变化看 data/credit-log.jsonl），
//     它的用途是 token 维度的成本归因。
//
// 刻意按【日期】分桶：不记日期就只能看"历史总量"，无法回答"今天用了多少"。
// 账号级与全局汇总都由明细 sum 得出（自然 rollup），不单独维护。
//
// 只观测：不参与选号/冷却/熔断任何决策，落盘失败也绝不影响主流程
// （与 creditStore / checkinStore 同约定）。
package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// tokenUsageKey 明细的聚合键：账号 × 模型 × 日期。
type tokenUsageKey struct {
	UID   string
	Model string
	Day   string // "2006-01-02"（本地时区）
}

// TokenDelta 单次请求的用量增量。负值表示"未知"（usage 缺失），累加时钳 0。
type TokenDelta struct {
	In     int // prompt_tokens（输入/上下文）
	Out    int // completion_tokens（输出，按上游口径已含思考）
	Think  int // reasoning_tokens / completion_thinking_tokens
	Cached int // 命中缓存的输入（取值位置见 logging.go 的 pickCached）
}

// TokenUsageRow 一行统计（明细或汇总）。
type TokenUsageRow struct {
	UID      string `json:"uid,omitempty"`
	Model    string `json:"model,omitempty"`
	Day      string `json:"day,omitempty"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
	Think    int64  `json:"think"`
	Cached   int64  `json:"cached"`
	Requests int64  `json:"requests"`
	// Missing 记录其中有多少次请求上游未返回 usage（token 列为未知，仅计次）。
	// 单独统计是为了让展示层能说明"这些请求的 token 未计入"，不静默失真。
	Missing int64 `json:"missing,omitempty"`
}

// TokenUsageStore 用量统计存储：线程安全 + 原子落盘。
type TokenUsageStore struct {
	mu   sync.Mutex
	path string
	rows map[tokenUsageKey]*TokenUsageRow
	// dirty 标记有未落盘变更（每请求都会 Record，频繁写盘无必要）。
	dirty bool
}

// NewTokenUsageStore 构建存储；path 为空表示不落盘（降级纯内存）。
func NewTokenUsageStore(path string) *TokenUsageStore {
	s := &TokenUsageStore{path: path, rows: map[tokenUsageKey]*TokenUsageRow{}}
	s.load()
	return s
}

// load 读取历史；文件缺失/损坏静默降级为空（不影响启动）。
func (s *TokenUsageStore) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var rows []TokenUsageRow
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	for i := range rows {
		r := rows[i]
		s.rows[tokenUsageKey{r.UID, r.Model, r.Day}] = &r
	}
}

// Record 累加一次请求的用量。uid/model 为空或负值增量按约定处理：
//   - token 增量为负（usage 缺失）→ 钳 0，仅累加 Requests 与 Missing；
//   - uid/model 为空 → 用占位符，保证请求数不漏计（宁可展示为"-"也不丢计数）。
func (s *TokenUsageStore) Record(uid, model string, at time.Time, d TokenDelta) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		uid = "-"
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = "-"
	}
	key := tokenUsageKey{UID: uid, Model: model, Day: at.Format("2006-01-02")}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key]
	if !ok {
		r = &TokenUsageRow{UID: key.UID, Model: key.Model, Day: key.Day}
		s.rows[key] = r
	}
	r.Requests++
	if d.In < 0 || d.Out < 0 {
		r.Missing++
	}
	r.In += clampNonNeg(d.In)
	r.Out += clampNonNeg(d.Out)
	r.Think += clampNonNeg(d.Think)
	r.Cached += clampNonNeg(d.Cached)
	s.dirty = true
}

// clampNonNeg 负值（未知）钳 0：未知不得污染总和，也不得让总和变小。
func clampNonNeg(v int) int64 {
	if v < 0 {
		return 0
	}
	return int64(v)
}

// Rows 返回明细副本，按日期倒序、账号升序、模型升序（顺序稳定便于展示与断言）。
func (s *TokenUsageStore) Rows() []TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedRows(s.rows, nil)
}

// RowsForDay 返回指定日期（"2006-01-02"）的明细。
func (s *TokenUsageStore) RowsForDay(day string) []TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedRows(s.rows, func(k tokenUsageKey) bool { return k.Day == day })
}

// ByAccount 账号级汇总（对明细 sum，自然 rollup）。
func (s *TokenUsageStore) ByAccount() []TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	agg := map[string]*TokenUsageRow{}
	for _, r := range s.rows {
		a, ok := agg[r.UID]
		if !ok {
			a = &TokenUsageRow{UID: r.UID}
			agg[r.UID] = a
		}
		addInto(a, r)
	}
	out := make([]TokenUsageRow, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// ByAccountForDay 指定日期的账号级汇总。
func (s *TokenUsageStore) ByAccountForDay(day string) []TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return aggregateBy(s.rows, true, day)
}

// ByModel 模型级汇总（对明细 sum，自然 rollup）。
func (s *TokenUsageStore) ByModel() []TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return aggregateBy(s.rows, false, "")
}

// ByModelForDay 指定日期的模型级汇总。
func (s *TokenUsageStore) ByModelForDay(day string) []TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return aggregateBy(s.rows, false, day)
}

// aggregateBy 按账号（byAccount=true）或模型（false）汇总；day 非空时只统计该日期。
// 结果按维度名升序（顺序稳定）。
func aggregateBy(rows map[tokenUsageKey]*TokenUsageRow, byAccount bool, day string) []TokenUsageRow {
	agg := map[string]*TokenUsageRow{}
	for k, r := range rows {
		if day != "" && k.Day != day {
			continue
		}
		name := k.Model
		if byAccount {
			name = k.UID
		}
		a, ok := agg[name]
		if !ok {
			a = &TokenUsageRow{}
			if byAccount {
				a.UID = name
			} else {
				a.Model = name
			}
			agg[name] = a
		}
		addInto(a, r)
	}
	out := make([]TokenUsageRow, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UID != out[j].UID {
			return out[i].UID < out[j].UID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Total 全局汇总。
func (s *TokenUsageStore) Total() TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return totalLocked(s.rows, "")
}

// TotalForDay 指定日期的全局汇总。
func (s *TokenUsageStore) TotalForDay(day string) TokenUsageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return totalLocked(s.rows, day)
}

// totalLocked 汇总所有（或指定日期的）明细。day 为空表示不筛日期。
func totalLocked(rows map[tokenUsageKey]*TokenUsageRow, day string) TokenUsageRow {
	var t TokenUsageRow
	for _, r := range rows {
		if day != "" && r.Day != day {
			continue
		}
		addInto(&t, r)
	}
	return t
}

// addInto 把 src 的各项累加到 dst（含请求数与缺 usage 计数）。
func addInto(dst, src *TokenUsageRow) {
	dst.In += src.In
	dst.Out += src.Out
	dst.Think += src.Think
	dst.Cached += src.Cached
	dst.Requests += src.Requests
	dst.Missing += src.Missing
}

// sortedRows 把 map 转为切片并按 (日期倒序, 账号, 模型) 排序；filter 可为 nil。
func sortedRows(rows map[tokenUsageKey]*TokenUsageRow, filter func(tokenUsageKey) bool) []TokenUsageRow {
	out := make([]TokenUsageRow, 0, len(rows))
	for k, r := range rows {
		if filter != nil && !filter(k) {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day // 日期倒序（最新在前）
		}
		if out[i].UID != out[j].UID {
			return out[i].UID < out[j].UID
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Flush 把当前统计落盘（原子写 tmp+rename；失败仅忽略——属观测数据）。
func (s *TokenUsageStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

// flushLocked 落盘。调用方需已持锁。
func (s *TokenUsageStore) flushLocked() {
	if s.path == "" || !s.dirty {
		return
	}
	rows := sortedRows(s.rows, nil)
	raw, err := json.MarshalIndent(rows, "", "  ")
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
	if err := os.Rename(tmp, s.path); err != nil {
		return
	}
	s.dirty = false
}

// Close 落盘并关闭（幂等）。
func (s *TokenUsageStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}
