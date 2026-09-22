package upstream

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// captureRT 记录最近一次请求的方法/路径/请求头/请求体，并返回预设响应。
type captureRT struct {
	method, path string
	header       http.Header
	body         []byte
	status       int
	respBody     string
}

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.method, c.path, c.header = r.Method, r.URL.Path, r.Header
	if r.Body != nil {
		c.body, _ = io.ReadAll(r.Body)
	}
	return &http.Response{
		StatusCode: c.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(c.respBody)),
	}, nil
}

func imageClient(rt http.RoundTripper) *Client {
	return &Client{
		HTTP:        &http.Client{Transport: rt},
		ChatBaseCN:  "https://fake.example",
		proxyPool:   newProxyPool(0),
	}
}

// TestGenerateImageHunyuanBody hunyuan 族请求体形状：n/footnote/revise{value}，
// 不带 response_format；并校验鉴权头与 X-Model-ID。
func TestGenerateImageHunyuanBody(t *testing.T) {
	rt := &captureRT{status: 200, respBody: `{"code":0,"data":{"data":[{"url":"https://img.example/a.png","revised_prompt":"rp"}]}}`}
	c := imageClient(rt)
	revise := true
	items, err := c.GenerateImage(&auth.Auth{UID: "u1", AccessToken: "tok", EnterpriseID: "ent"}, ImageRequest{
		Model: "hunyuan-image-alpha", Prompt: "a cat", Size: "1024x1024", N: 2,
		Footnote: "fn", Revise: &revise,
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if rt.path != "/v2/images/generations" || rt.method != http.MethodPost {
		t.Fatalf("unexpected target %s %s", rt.method, rt.path)
	}
	if got := rt.header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization=%q", got)
	}
	if got := rt.header.Get("X-Model-ID"); got != "hunyuan-image-alpha" {
		t.Fatalf("X-Model-ID=%q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rt.body, &body); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if body["model"] != "hunyuan-image-alpha" || body["prompt"] != "a cat" || body["size"] != "1024x1024" {
		t.Fatalf("body=%v", body)
	}
	if body["n"].(float64) != 2 || body["footnote"] != "fn" {
		t.Fatalf("hunyuan fields missing: %v", body)
	}
	if rv, ok := body["revise"].(map[string]any); !ok || rv["value"] != true {
		t.Fatalf("revise shape wrong: %v", body["revise"])
	}
	if _, has := body["response_format"]; has {
		t.Fatalf("hunyuan body must not carry response_format: %v", body)
	}
	if len(items) != 1 || items[0].URL != "https://img.example/a.png" || items[0].RevisedPrompt != "rp" {
		t.Fatalf("items=%+v", items)
	}
}

// TestGenerateImageNonHunyuanBody 非 hunyuan 族：response_format=b64_json + quality/style/background。
func TestGenerateImageNonHunyuanBody(t *testing.T) {
	rt := &captureRT{status: 200, respBody: `{"code":0,"data":{"data":[{"b64_json":"QUJD"}]}}`}
	c := imageClient(rt)
	items, err := c.GenerateImage(&auth.Auth{UID: "u1", AccessToken: "tok"}, ImageRequest{
		Model: "gpt-image-1", Prompt: "p", Size: "1024x1024", N: 1,
		Quality: "high", Style: "vivid", Background: "transparent",
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(rt.body, &body)
	if body["response_format"] != "b64_json" || body["quality"] != "high" || body["style"] != "vivid" || body["background"] != "transparent" {
		t.Fatalf("body=%v", body)
	}
	if len(items) != 1 || items[0].B64JSON != "QUJD" {
		t.Fatalf("items=%+v", items)
	}
}

// TestEditImageBody edits 端点携带 image 数组与 input_fidelity。
func TestEditImageBody(t *testing.T) {
	rt := &captureRT{status: 200, respBody: `{"code":0,"data":{"data":[{"url":"https://img.example/e.png"}]}}`}
	c := imageClient(rt)
	_, err := c.EditImage(&auth.Auth{UID: "u1", AccessToken: "tok"}, ImageRequest{
		Model: "hunyuan-image-v2.0-general-edit", Prompt: "make it blue", Size: "1024x1024",
		Images: []string{"data:image/png;base64,QUJD"}, InputFidelity: "high",
	})
	if err != nil {
		t.Fatalf("EditImage: %v", err)
	}
	if rt.path != "/v2/images/edits" {
		t.Fatalf("path=%s", rt.path)
	}
	var body map[string]any
	_ = json.Unmarshal(rt.body, &body)
	imgs, _ := body["image"].([]any)
	if len(imgs) != 1 || imgs[0] != "data:image/png;base64,QUJD" || body["input_fidelity"] != "high" {
		t.Fatalf("body=%v", body)
	}
}

// TestImageAPIError 业务 code != 0 归类为 *Error（429 → ErrSoftRate）。
func TestImageAPIError(t *testing.T) {
	rt := &captureRT{status: 429, respBody: `{"code":14003,"msg":"Hunyuan image queue is full for model [hunyuan-image-alpha]"}`}
	c := imageClient(rt)
	_, err := c.GenerateImage(&auth.Auth{UID: "u1", AccessToken: "tok"}, ImageRequest{Model: "hunyuan-image-alpha", Prompt: "p", Size: "1024x1024"})
	ue, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if ue.Kind != ErrSoftRate || ue.Status != 429 {
		t.Fatalf("kind=%s status=%d", ue.Kind, ue.Status)
	}
}

func TestNormalizeImageInputs(t *testing.T) {
	pngB64 := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 1, 2, 3})

	// http URL → 抓取转 data URL
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	}))
	defer imgSrv.Close()

	// 归一化测试需要真实打 httptest 的 imgSrv，故用默认 Transport 而非 captureRT。
	c := &Client{HTTP: &http.Client{}, ChatBaseCN: "https://fake.example", proxyPool: newProxyPool(0)}
	out, err := c.NormalizeImageInputs(nil, []string{
		"data:image/png;base64,QUJD",
		imgSrv.URL + "/x.png",
		pngB64,
	})
	if err != nil {
		t.Fatalf("NormalizeImageInputs: %v", err)
	}
	if out[0] != "data:image/png;base64,QUJD" {
		t.Fatalf("data url mangled: %s", out[0][:30])
	}
	if !strings.HasPrefix(out[1], "data:image/png;base64,") {
		t.Fatalf("remote not normalized: %s", out[1][:40])
	}
	if !strings.HasPrefix(out[2], "data:image/png;base64,") || !strings.HasSuffix(out[2], pngB64) {
		t.Fatalf("raw b64 not wrapped: %s", out[2][:40])
	}

	// 本地路径拒绝
	if _, err := c.NormalizeImageInputs(nil, []string{"C:\\tmp\\x.png"}); err == nil {
		t.Fatal("windows path should be rejected")
	}
	if _, err := c.NormalizeImageInputs(nil, []string{"/etc/passwd"}); err == nil {
		t.Fatal("unix path should be rejected")
	}
	// 非 base64 垃圾拒绝
	if _, err := c.NormalizeImageInputs(nil, []string{"!!!notbase64!!!"}); err == nil {
		t.Fatal("garbage should be rejected")
	}
}
