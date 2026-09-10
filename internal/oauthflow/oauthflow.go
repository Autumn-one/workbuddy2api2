// login.go — WorkBuddy CN OAuth 登录（设备授权流程，CN realm only）。
//
// 本包由 cmd/login（CLI，供 login.sh / login.ps1 调用）与 cmd/gui 共同使用。
//
//	login url   → POST /v2/plugin/auth/state?platform=CLI 拿 state+authUrl，
//	              state 落系统临时目录（os.TempDir()），stdout 打印授权 URL
//	login poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	              成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	              stdout 打印完整 token+account JSON
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package oauthflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"time"
)

// 上游常量（CN only）
const (
	upstreamBaseCN    = "https://copilot.tencent.com"
	clientUA          = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer     = "https://www.codebuddy.cn"
	endpointAuthState = upstreamBaseCN + "/v2/plugin/auth/state?platform=CLI"
	endpointLoginAcct = upstreamBaseCN + "/v2/plugin/login/account?state="
	endpointAuthToken = upstreamBaseCN + "/v2/plugin/auth/token?state="
)

// stateFile 跨平台临时路径。原先硬编码 "/tmp/..."，在 Windows 上会被解析为
// 当前盘符根目录下的 \tmp\...，而该目录通常不存在且 os.WriteFile 不会建父目录，
// 导致 `login url` 直接失败。用 os.TempDir() 同时覆盖 Linux(/tmp) 与 Windows(%TEMP%)。
var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

// commonHeaders 通用请求头
func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

type loginState struct {
	State string `json:"state"`
}

// ErrPending 表示登录尚未完成（用户在浏览器里还没点完），可稍后重试。
var ErrPending = errors.New("login_pending")

// Bundle 登录成功后拿到的完整凭证。
// JSON 标签刻意与 cmd/login 原 stdout 输出保持一致（snake_case），供脚本解析。
type Bundle struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
}

// Client 承载一次登录流程（独立 cookie jar，多账号登录互不串会话）。
type Client struct {
	http *http.Client
}

// NewClient 构造登录客户端。
func NewClient() *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{http: &http.Client{Timeout: 30 * time.Second, Jar: jar}}
}

// StartLogin 发起设备授权，返回授权 URL；state 落临时文件，供 PollLogin 读取。
func (c *Client) StartLogin() (string, error) {
	data, _, err := doJSON(c.http, http.MethodPost, endpointAuthState, nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", errors.New("auth state: missing state or authUrl")
	}
	raw, _ := json.Marshal(loginState{State: st.State})
	if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
		return "", fmt.Errorf("write state: %w", err)
	}
	return st.AuthURL, nil
}

// PollLogin 拉取一次登录结果：完成返回 Bundle；未完成返回 ErrPending，可稍后重试。
func (c *Client) PollLogin() (*Bundle, error) {
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("read state: %w (先跑 StartLogin)", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
	// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle
	tokRaw, status, errTok := doJSON(c.http, http.MethodGet, endpointAuthToken+ls.State, nil, nil)
	if errTok != nil {
		if status == 0 || status >= 500 {
			return nil, fmt.Errorf("token endpoint error: %w", errTok)
		}
		return nil, ErrPending
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrPending
	}
	// login/account 拿 uid/nickname（带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(c.http, http.MethodGet, endpointLoginAcct+ls.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	os.Remove(stateFile)
	return &Bundle{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
	}, nil
}

// SaveAuthFile 把凭证写入 dir/workbuddy-<uid>.json，结构见 internal/auth 的嵌套形。
// 返回写入路径与是否覆盖了已有文件。
func SaveAuthFile(dir string, b *Bundle) (string, bool, error) {
	if b.UID == "" {
		return "", false, errors.New("uid 为空，无法落盘")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	path := filepath.Join(dir, "workbuddy-"+b.UID+".json")
	_, statErr := os.Stat(path)
	overwritten := statErr == nil

	doc := map[string]any{
		"account": map[string]string{
			"uid":          b.UID,
			"enterpriseId": b.EnterpriseID,
			"nickname":     b.Nickname,
		},
		"auth": map[string]any{
			"accessToken":  b.AccessToken,
			"refreshToken": b.RefreshToken,
			"expiresAt":    time.Now().Unix() + b.ExpiresIn,
			"domain":       b.Domain,
		},
	}
	raw, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", false, err
	}
	return path, overwritten, nil
}

// StateFilePath 返回共享的 state 临时文件路径（GUI 可用于清理）。
func StateFilePath() string { return stateFile }
