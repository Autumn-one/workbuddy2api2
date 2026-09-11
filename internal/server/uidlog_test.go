package server

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestLogAccountNamePrefersNickname 请求日志的账号列显示【昵称前 7 字符】，
// 让人不用查 auths 文件就知道是哪个账号。
func TestLogAccountNamePrefersNickname(t *testing.T) {
	cases := []struct {
		name string
		a    *auth.Auth
		want string
	}{
		{"昵称优先", &auth.Auth{UID: "4bfda6dd-1717", Nickname: "木瓜"}, "木瓜"},
		{"长昵称截 7 字符", &auth.Auth{UID: "d44304c5", Nickname: "19396392726"}, "1939639"},
		{"无昵称回落 UID 前 8 位", &auth.Auth{UID: "00e26541abcdef"}, "00e26541"},
		{"空 uid 显示 -", &auth.Auth{}, "-"},
		{"空昵称但有 uid", &auth.Auth{UID: "abc"}, "abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := logAccountName(c.a); got != c.want {
				t.Errorf("logAccountName(%v)=%q want %q", c.a, got, c.want)
			}
		})
	}
}

// TestLogChatRowShowsNickname 端到端：请求行里出现昵称而非 UID。
func TestLogChatRowShowsNickname(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync",
			&auth.Auth{UID: "d44304c5-xx", Nickname: "19396392726"},
			200, 10, 5, 3, 3, upstream.EffectiveParams{})
	})
	if !strings.Contains(out, "1939639") {
		t.Errorf("请求行应显示昵称前 7 字符: %s", out)
	}
	if strings.Contains(out, "acct=d44304c5") {
		t.Errorf("昵称存在时不应回落 UID: %s", out)
	}
}
