// modelprobe 一次性探针：拉上游 /console/enterprises/personal/models 全量目录，
// 打印所有 agent 分组与全部模型（含非 cli 组、disabled、text-to-image 等）。
//
// 只读、不刷新 token、不写回凭证。挑 ExpiresAt 最靠后的账号直接用现有 accessToken。
// 用法：go run ./modelprobe
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

type dynModel struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	MaxInputTokens    int64    `json:"maxInputTokens"`
	MaxOutputTokens   int64    `json:"maxOutputTokens"`
	Disabled          bool     `json:"disabled"`
	Credits           string   `json:"credits"`
	DescriptionZh     string   `json:"descriptionZh"`
	Vendor            string   `json:"vendor"`
	SupportsImages    bool     `json:"supportsImages"`
	SupportsReasoning bool     `json:"supportsReasoning"`
	SupportsToolCall  bool     `json:"supportsToolCall"`
	Tags              []string `json:"tags"`
	IsDefault         bool     `json:"isDefault"`
}

func main() {
	auths, bad, err := auth.LoadDir("auths")
	if err != nil {
		log.Fatalf("LoadDir: %v", err)
	}
	for _, b := range bad {
		log.Printf("bad auth file: %s: %v", b.Path, b.Err)
	}
	// 挑 expiresAt 最大（最可能仍有效）的账号；不刷新、不写回。
	var pick *auth.Auth
	for _, a := range auths {
		if pick == nil || a.ExpiresAt > pick.ExpiresAt {
			pick = a
		}
	}
	if pick == nil {
		log.Fatal("no accounts")
	}
	fmt.Printf("using uid=%s nick=%s expiresAt=%s (in %s)\n\n",
		pick.UID, pick.Nickname,
		time.Unix(pick.ExpiresAt, 0).Format("2006-01-02 15:04"),
		time.Until(time.Unix(pick.ExpiresAt, 0)).Round(time.Minute))

	req, _ := http.NewRequest(http.MethodGet,
		"https://copilot.tencent.com/console/enterprises/personal/models", nil)
	req.Header.Set("Authorization", "Bearer "+pick.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://www.codebuddy.cn")
	req.Header.Set("Referer", "https://www.codebuddy.cn/")
	req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		log.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	fmt.Printf("HTTP %d, %d bytes\n\n", resp.StatusCode, len(raw))
	if resp.StatusCode != 200 {
		fmt.Println(truncate(string(raw), 500))
		return
	}

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Models []dynModel `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Fatalf("parse: %v\n%s", err, truncate(string(raw), 300))
	}
	fmt.Printf("code=%d msg=%q\n\n", env.Code, env.Msg)

	inAgent := map[string][]string{}
	fmt.Println("=== agents ===")
	for _, ag := range env.Data.Agents {
		fmt.Printf("agent %q: %d models\n", ag.Name, len(ag.Models))
		for _, id := range ag.Models {
			inAgent[id] = append(inAgent[id], ag.Name)
		}
	}
	fmt.Println()

	fmt.Printf("=== all models (%d) ===\n", len(env.Data.Models))
	sort.Slice(env.Data.Models, func(i, j int) bool { return env.Data.Models[i].ID < env.Data.Models[j].ID })
	for _, m := range env.Data.Models {
		flags := ""
		if m.Disabled {
			flags += " DISABLED"
		}
		if m.IsDefault {
			flags += " DEFAULT"
		}
		if m.SupportsImages {
			flags += " img-in"
		}
		if m.SupportsToolCall {
			flags += " tools"
		}
		if m.SupportsReasoning {
			flags += " reasoning"
		}
		fmt.Printf("%-28s agents=%v out=%-6d tags=%v credits=%q%s\n  name=%q desc=%q\n",
			m.ID, inAgent[m.ID], m.MaxOutputTokens, m.Tags, m.Credits, flags,
			m.Name, m.DescriptionZh)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
