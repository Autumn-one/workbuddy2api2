// Package upstream 图像生成链路（文生图 / 图生图编辑）。
//
// 上游契约（对齐 WorkBuddy ImageService.buildBaseRequestBody，并经同源实现实测）：
//   - POST {chatBase}/v2/images/generations —— 文生图
//   - POST {chatBase}/v2/images/edits       —— 图生图编辑
//
// 请求体按模型族分两套装配：
//   - hunyuan-*：{model, prompt, size, n, footnote?, revise:{value}}（返回 url）
//   - 其他模型：{model, prompt, size, n, response_format:"b64_json",
//     quality?, style?, background?}
//
// 响应信封：{"code":0,"data":{"data":[{"url"|"b64_json", "revised_prompt"?}]}}，非流式。
//
// 已知上游限制：混元生图并发槽仅 2，满载返回 429/code=14003 "queue is full"——
// 该错误经 Classify 归为 ErrSoftRate，服务端按账号轮转 + 软冷却处理。
package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

// ImageRequest 图像请求的解析结果（服务端层完成 OpenAI 字段 → 本结构的映射）。
type ImageRequest struct {
	Model  string
	Prompt string
	Size   string // 形如 "1024x1024"；空串由服务端补默认值
	N      int    // <=0 按 1 处理

	// hunyuan 族专属
	Footnote string // 图片脚注文案（可选）
	Revise   *bool  // 提示词自动润色开关；nil = 不传

	// 非 hunyuan 族专属
	Quality    string
	Style      string
	Background string

	// edits 专用
	Images        []string // 已归一化为 data URL 的输入图
	InputFidelity string   // 图生图输入保真度档位（可选）
}

// ImageItem 上游返回的单张图像结果。
type ImageItem struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// imageData data 层形状：{"data":[{"url"|"b64_json",...}]}。
type imageData struct {
	Data []ImageItem `json:"data"`
}

// GenerateImage 文生图：POST /v2/images/generations。
// 返回值含【实际使用】的出口代理（空串 = 直连），供请求日志展示。
func (c *Client) GenerateImage(a *auth.Auth, in ImageRequest) ([]ImageItem, string, error) {
	return c.postImage(a, "/v2/images/generations", in, false)
}

// EditImage 图生图编辑：POST /v2/images/edits。in.Images 必须已是 data URL。
// 返回值含【实际使用】的出口代理（空串 = 直连），供请求日志展示。
func (c *Client) EditImage(a *auth.Auth, in ImageRequest) ([]ImageItem, string, error) {
	return c.postImage(a, "/v2/images/edits", in, true)
}

// postImage 发图像请求并解析信封。错误路径沿用 doJSONFor 的 *Error（已含 Classify 分类）。
// 返回的 proxy 是本次请求实际使用的出口代理（含不可达回落后的真实值）。
func (c *Client) postImage(a *auth.Auth, path string, in ImageRequest, edit bool) ([]ImageItem, string, error) {
	n := in.N
	if n <= 0 {
		n = 1
	}
	body := map[string]any{
		"model":  in.Model,
		"prompt": in.Prompt,
		"size":   in.Size,
		"n":      n,
	}
	if strings.HasPrefix(in.Model, "hunyuan-") {
		if in.Footnote != "" {
			body["footnote"] = in.Footnote
		}
		if in.Revise != nil {
			body["revise"] = map[string]any{"value": *in.Revise}
		}
	} else {
		body["response_format"] = "b64_json"
		if in.Quality != "" {
			body["quality"] = in.Quality
		}
		if in.Style != "" {
			body["style"] = in.Style
		}
		if in.Background != "" {
			body["background"] = in.Background
		}
	}
	if edit {
		body["image"] = in.Images
		if in.InputFidelity != "" {
			body["input_fidelity"] = in.InputFidelity
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+path, bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	ChatHeaders(req, a)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Model-ID", in.Model)
	data, proxy, err := c.doJSONForWithProxy(a, req)
	if err != nil {
		return nil, proxy, err
	}
	var d imageData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, proxy, fmt.Errorf("image parse: %w (body: %s)", err, truncate(string(data), 120))
	}
	items := d.Data[:0]
	for _, it := range d.Data {
		if it.URL != "" || it.B64JSON != "" {
			items = append(items, it)
		}
	}
	return items, proxy, nil
}

// maxImageFetchBytes 拉取远程输入图/结果图的大小上限（防超大响应撑爆内存）。
const maxImageFetchBytes = 20 << 20

// InlineImageURLs 把 items 里的 url 结果抓取后转为 b64_json（原 url 保留）。
// 用于客户端指定 response_format=b64_json、而上游（hunyuan 族）只回 url 的场景。
// 单张失败不致命：该条目保留 url 原样返回。调用方按 n 限制数量，串行抓取即可。
func (c *Client) InlineImageURLs(a *auth.Auth, items []ImageItem) {
	for i := range items {
		if items[i].B64JSON != "" || items[i].URL == "" {
			continue
		}
		// b64_json 按 OpenAI 语义只含裸 base64，MIME 无需带出。
		d, _, err := c.fetchRemoteBytes(a, items[i].URL)
		if err != nil {
			log.Printf("image inline %s: %v", truncate(items[i].URL, 60), err)
			continue
		}
		items[i].B64JSON = base64.StdEncoding.EncodeToString(d)
	}
}

// NormalizeImageInputs 把 edits 的 image 输入归一化为 data URL 列表
// （对齐 WorkBuddy processImageInput）：
//   - "data:..."        → 原样透传；
//   - "http(s)://..."   → 网关侧抓取并转 base64 data URL（上游拒收远程 URL）；
//   - 其他              → 按裸 base64 处理，按魔数探测 MIME。
//
// 不支持本地文件路径——否则 API 客户端可令网关读取服务器任意文件。
// a 可为 nil（直连抓取）；非 nil 时走该账号的代理出口。
func (c *Client) NormalizeImageInputs(a *auth.Auth, inputs []string) ([]string, error) {
	out := make([]string, 0, len(inputs))
	for _, in := range inputs {
		s := strings.TrimSpace(in)
		switch {
		case strings.HasPrefix(s, "data:"):
			out = append(out, s)
		case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
			d, mime, err := c.fetchRemoteBytes(a, s)
			if err != nil {
				return nil, fmt.Errorf("fetch image %s: %w", truncate(s, 60), err)
			}
			out = append(out, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(d))
		case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "."),
			strings.Contains(s, ":\\"), strings.Contains(s, "file:"):
			return nil, fmt.Errorf("local file paths are not supported for image input")
		default:
			d, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, fmt.Errorf("image input is neither data URL, http(s) URL, nor valid base64")
			}
			out = append(out, "data:"+sniffMIME(d)+";base64,"+s)
		}
	}
	return out, nil
}

// fetchRemoteBytes 拉取远程资源并按 Content-Type/魔数确定 MIME。
func (c *Client) fetchRemoteBytes(a *auth.Auth, url string) (data []byte, mime string, err error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.clientFor(a, false).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxImageFetchBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxImageFetchBytes {
		return nil, "", fmt.Errorf("image exceeds %d bytes", maxImageFetchBytes)
	}
	mime = resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if !strings.HasPrefix(mime, "image/") {
		mime = sniffMIME(data)
	}
	return data, mime, nil
}

// sniffMIME 按魔数识别常见图片格式，兜底 image/png。
func sniffMIME(b []byte) string {
	if ct := http.DetectContentType(b); strings.HasPrefix(ct, "image/") {
		return ct
	}
	return "image/png"
}
