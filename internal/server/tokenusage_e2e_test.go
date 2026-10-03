package server

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// usage 里带全部字段的 SSE（含 cached_tokens）。
const sseWithCache = "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
	"\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":50," +
	"\"cached_tokens\":800,\"cache_read_input_tokens\":800," +
	"\"completion_tokens_details\":{\"reasoning_tokens\":20}}}\n\ndata: [DONE]\n\n"

func TestUsageRecordedEndToEnd(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseWithCache, true })
	store := NewTokenUsageStore("")
	defer store.Close()
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", Nickname: "木瓜", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		UsageStore: store,
	})
	out := captureStdout(t, func() {
		rec := httptestPost(h, `{"model":"glm-5.2","messages":[]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	if !strings.Contains(out, "cache=800") {
		t.Errorf("请求行应含 cache=800: %s", out)
	}
	if !strings.Contains(out, "ctx=1000") || !strings.Contains(out, "tok=50") {
		t.Errorf("ctx/tok 应正确: %s", out)
	}
	rows := store.Rows()
	if len(rows) != 1 {
		t.Fatalf("应记录 1 行, got %d", len(rows))
	}
	r := rows[0]
	if r.UID != "u1" || r.Model != "glm-5.2" {
		t.Errorf("键错误: %+v", r)
	}
	if r.In != 1000 || r.Out != 50 || r.Think != 20 || r.Cached != 800 {
		t.Errorf("用量错误: %+v", r)
	}
	if r.Requests != 1 || r.Missing != 0 {
		t.Errorf("请求数/缺失计数错误: %+v", r)
	}
	day := r.Day
	if len(day) != 10 {
		t.Errorf("日期格式应 YYYY-MM-DD, got %q", day)
	}
	// 账号级与全局汇总
	if acct := store.ByAccount(); len(acct) != 1 || acct[0].In != 1000 {
		t.Errorf("账号级汇总错误: %+v", acct)
	}
	if tot := store.Total(); tot.In != 1000 || tot.Requests != 1 {
		t.Errorf("全局汇总错误: %+v", tot)
	}
	t.Logf("记录: day=%s uid=%s model=%s in=%d out=%d think=%d cached=%d",
		r.Day, r.UID, r.Model, r.In, r.Out, r.Think, r.Cached)
}

// 失败请求（6004）无 usage：仍计一次请求，token 计 0 且 Missing+1。
func TestUsageRecordedOnFailure(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 429, blockedBody, true })
	store := NewTokenUsageStore("")
	defer store.Close()
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		UsageStore: store,
	})
	httptestPost(h, `{"model":"deepseek-v4.1-flash","messages":[]}`)
	rows := store.Rows()
	if len(rows) == 0 {
		t.Fatal("失败请求也应记录（计请求数）")
	}
	r := rows[0]
	if r.Requests != 1 {
		t.Errorf("请求数=%d want 1", r.Requests)
	}
	if r.In != 0 || r.Out != 0 {
		t.Errorf("失败无 usage，token 应钳 0: %+v", r)
	}
	if r.Missing != 1 {
		t.Errorf("Missing=%d want 1（缺 usage 计数）", r.Missing)
	}
}

// usage 里缓存真值只在嵌套位置、顶层同名字段是 0 影子字段——实测上游形态（见 pickCached）。
const sseWithNestedCache = "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
	"\"usage\":{\"prompt_tokens\":313986,\"completion_tokens\":263,\"cached_tokens\":0," +
	"\"prompt_tokens_details\":{\"cached_tokens\":313344}}}\n\ndata: [DONE]\n\n"

// TestUsageRecordedNestedCacheEndToEnd 嵌套位置的缓存真值必须一路进到请求日志与用量统计。
//
// 回归：旧实现只读顶层 cached_tokens，读到影子 0 → 请求日志 cache= 列与
// 用量页「缓存输入」恒为 0，缓存复用率这一项完全失明。
// 流式与非流式两条出口都要覆盖——两者解析路径不同（SSE 末帧 struct / Aggregate map）。
func TestUsageRecordedNestedCacheEndToEnd(t *testing.T) {
	cases := []struct {
		name string
		body string
		mode string
	}{
		{"流式（SSE 末帧）", `{"model":"deepseek-v4.1-flash","stream":true,"messages":[]}`, "| stream |"},
		{"非流式（Aggregate）", `{"model":"deepseek-v4.1-flash","messages":[]}`, "| sync |"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withChatLog(t)
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseWithNestedCache, true })
			store := NewTokenUsageStore("")
			defer store.Close()
			h := NewHandler(Config{
				Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:   up,
				UsageStore: store,
			})
			out := captureStdout(t, func() {
				rec := httptestPost(h, c.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("code=%d", rec.Code)
				}
			})
			for _, want := range []string{c.mode, "cache=313344", "ctx=313986", "tok=263"} {
				if !strings.Contains(out, want) {
					t.Errorf("请求行缺少 %q:\n%s", want, out)
				}
			}
			rows := store.Rows()
			if len(rows) != 1 {
				t.Fatalf("应记录 1 行, got %d", len(rows))
			}
			if r := rows[0]; r.Cached != 313344 || r.In != 313986 || r.Out != 263 {
				t.Errorf("用量记录错误: %+v", r)
			}
		})
	}
}
