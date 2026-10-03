package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// newFakeUpstreamByPath 与 newFakeUpstream 同构，但 behavior 能看到 URL path
// （区分 /v2/chat/completions 与 /v2/images/*）。
func newFakeUpstreamByPath(behavior func(authz, path string) (status int, body string)) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body := behavior(r.Header.Get("Authorization"), r.URL.Path)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func TestImageGenerationsOK(t *testing.T) {
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		if path != "/v2/images/generations" {
			t.Fatalf("unexpected path %s", path)
		}
		return 200, `{"code":0,"data":{"data":[{"url":"https://img.example/a.png","revised_prompt":"rp"}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"prompt":"a cat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].URL != "https://img.example/a.png" || out.Data[0].RevisedPrompt != "rp" {
		t.Fatalf("out=%+v", out)
	}
}

// TestImageFootnoteDefault 生图 footnote 默认注入：
// 配置了 ImageFootnoteDefault 时客户端未传 footnote → 注入默认值（含空串）；
// 客户端显式传 footnote 时优先于默认。
func TestImageFootnoteDefault(t *testing.T) {
	var gotBody []byte
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		return 200, `{"code":0,"data":{"data":[{"url":"https://img.example/a.png"}]}}`
	})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"data":[{"url":"https://img.example/a.png"}]}}`)),
		}, nil
	})
	def := ""
	h := NewHandler(Config{
		Pool:                 testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:             up,
		ImageFootnoteDefault: &def,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	post := func(body string) map[string]any {
		resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, b)
		}
		var reqBody map[string]any
		if err := json.Unmarshal(gotBody, &reqBody); err != nil {
			t.Fatal(err)
		}
		return reqBody
	}

	// 客户端未传 → 注入默认空串（footnote:"" 必须出现在请求体里）
	if b := post(`{"prompt":"a cat"}`); b["footnote"] != "" {
		t.Fatalf("default footnote not injected: %v", b)
	}
	// 客户端显式传 → 客户端优先
	if b := post(`{"prompt":"a cat","footnote":"my-mark"}`); b["footnote"] != "my-mark" {
		t.Fatalf("client footnote must win: %v", b)
	}
}

// TestImageFootnoteUnset 未配置默认时客户端不传 footnote → 请求体不带该键。
func TestImageFootnoteUnset(t *testing.T) {
	var gotBody []byte
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		return 200, `{"code":0,"data":{"data":[{"url":"https://img.example/a.png"}]}}`
	})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"data":[{"url":"https://img.example/a.png"}]}}`)),
		}, nil
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"prompt":"a cat"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var reqBody map[string]any
	_ = json.Unmarshal(gotBody, &reqBody)
	if _, has := reqBody["footnote"]; has {
		t.Fatalf("footnote must be absent without client value and default: %v", reqBody)
	}
}

// TestImageGenerationsLogsRow 图像请求必须走与 chat 相同的表格日志链路——
// 曾因未接 chatStat 导致生图请求在运行日志/请求日志里完全不可见（回归测试）。
func TestImageGenerationsLogsRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		return 200, `{"code":0,"data":{"data":[{"url":"https://img.example/a.png"}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	out := captureStdout(t, func() {
		resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
			strings.NewReader(`{"prompt":"a cat"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d", resp.StatusCode)
		}
	})
	for _, want := range []string{"| #", defaultImageGenModel, "| image |", "| 200 |", "acct=u1", "total="} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
}

// TestImageEditsLogsRow edits 端点落 mode=imgedit 的表格行；校验失败也落行（status=400）。
func TestImageEditsLogsRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		return 200, `{"code":0,"data":{"data":[{"url":"https://img.example/e.png"}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	pngB64 := "iVBORw0KGgoAAAANSUhEUg=="
	out := captureStdout(t, func() {
		resp, err := http.Post(srv.URL+"/v1/images/edits", "application/json",
			strings.NewReader(`{"prompt":"make it blue","image":"`+pngB64+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		resp2, err := http.Post(srv.URL+"/v1/images/edits", "application/json",
			strings.NewReader(`{"prompt":"p"}`)) // 缺 image → 400
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
	})
	if !strings.Contains(out, "| imgedit |") {
		t.Errorf("missing imgedit row:\n%s", out)
	}
	if !strings.Contains(out, "| 400 |") {
		t.Errorf("validation failure should still log a row:\n%s", out)
	}
}

// TestImageGenerationsRotatesOn429 429 queue-full → 换号重试成功。
func TestImageGenerationsRotatesOn429(t *testing.T) {
	var calls atomic.Int32
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		calls.Add(1)
		if authz == "Bearer at-bad" {
			return 429, `{"code":14003,"msg":"Hunyuan image queue is full for model [hunyuan-image-alpha], please try again later"}`
		}
		return 200, `{"code":0,"data":{"data":[{"url":"https://img.example/ok.png"}]}}`
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"prompt":"a cat","model":"hunyuan-image-alpha"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 upstream calls (rotate), got %d", calls.Load())
	}
}

// TestImageGenerationsValidation prompt 缺失 / edits 缺 image → 400，不打上游。
func TestImageGenerationsValidation(t *testing.T) {
	var calls atomic.Int32
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		calls.Add(1)
		return 200, `{"code":0,"data":{"data":[{"url":"x"}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, tc := range []struct{ path, body string }{
		{"/v1/images/generations", `{}`},
		{"/v1/images/generations", `{"prompt":"  "}`},
		{"/v1/images/edits", `{"prompt":"p"}`},                         // 缺 image
		{"/v1/images/edits", `{"prompt":"p","image":"C:\\x.png"}`},     // 本地路径
		{"/v1/images/edits", `{"prompt":"p","image":"!!!garbage!!!"}`}, // 非 base64
	} {
		resp, err := http.Post(srv.URL+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%s %s: status=%d", tc.path, tc.body, resp.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream must not be called on validation failure, got %d", calls.Load())
	}
}

// TestImageEditsOK edits 正常路径：base64 输入被归一化为 data URL。
func TestImageEditsOK(t *testing.T) {
	var gotBody []byte
	up := newFakeUpstreamByPath(func(authz, path string) (int, string) {
		return 200, `{"code":0,"data":{"data":[{"url":"https://img.example/e.png"}]}}`
	})
	// 捕获请求体需要更底层的 fake——直接用 RoundTripper。
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"data":[{"url":"https://img.example/e.png"}]}}`)),
		}, nil
	})
	pngB64 := "iVBORw0KGgoAAAANSUhEUg==" // PNG 魔数前缀
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/images/edits", "application/json",
		strings.NewReader(`{"prompt":"make it blue","image":"`+pngB64+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var reqBody map[string]any
	if err := json.Unmarshal(gotBody, &reqBody); err != nil {
		t.Fatal(err)
	}
	if reqBody["model"] != defaultImageEditModel {
		t.Fatalf("default model=%v", reqBody["model"])
	}
	imgs, _ := reqBody["image"].([]any)
	if len(imgs) != 1 || imgs[0] != "data:image/png;base64,"+pngB64 {
		t.Fatalf("image not normalized: %v", reqBody["image"])
	}
}
