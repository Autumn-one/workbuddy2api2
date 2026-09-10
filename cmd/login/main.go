// login.go — CLI 外壳（OAuth 登录），由 login.sh / login.ps1 顺序驱动。
//
//	login url   → POST /v2/plugin/auth/state?platform=CLI 拿 state+authUrl，
//	              state 落系统临时目录，stdout 打印授权 URL
//	login poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	              成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	              stdout 打印完整 token+account JSON
//
// 真正的逻辑在 internal/oauthflow，与 GUI（cmd/gui）共用同一份实现。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"workbuddy2api/internal/oauthflow"
)

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll>")
	}
	c := oauthflow.NewClient()

	switch os.Args[1] {
	case "url":
		authURL, err := c.StartLogin()
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(authURL)

	case "poll":
		b, err := c.PollLogin()
		if err != nil {
			if errors.Is(err, oauthflow.ErrPending) {
				fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
			}
			fatal("%v", err)
		}
		raw, _ := json.Marshal(b)
		fmt.Println(string(raw))

	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}
