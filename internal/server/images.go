// 图像生成接口：OpenAI 兼容的 /v1/images/generations（文生图）与 /v1/images/edits（图生图）。
//
// 上游契约见 internal/upstream/images.go。轮转/鉴权/错误处置复用 chat 同一套
// pool 策略（Pick → Acquire → 续签 → 失败分类处置），但不做会话粘性——
// 图像请求无多轮语义，粘性只会减少可用账号面。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// 缺省模型：generations 用当前上游目录里的 text-to-image 模型；
// edits 用上 source/同源实现实测过的通用编辑模型（当前目录未单独列出，
// 客户端可用 model 字段覆盖）。
const (
	defaultImageGenModel  = "hunyuan-image-alpha"
	defaultImageEditModel = "hunyuan-image-v2.0-general-edit"
)

// imageGenRequest 客户端请求体：OpenAI images 标准字段 + 上游扩展字段透传。
type imageGenRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	Style          string `json:"style"`
	Background     string `json:"background"`
	ResponseFormat string `json:"response_format"`
	User           string `json:"user"`
	// 上游扩展（hunyuan 族）
	// Footnote 指针三态：缺键 = 不发（上游默认水印）；"" = 显式空串（试探关水印）；
	// 非空 = 自定义水印文案（≤16 字符，右下角）。
	Footnote *string `json:"footnote"`
	Revise   *bool   `json:"revise"`
	// Extra 实验性上游字段透传（仅 hunyuan 族；用于试 logo_add 等候选水印开关）。
	Extra map[string]any `json:"extra"`
	// edits 专用：image 可为字符串或字符串数组
	// （data URL / http(s) URL / 裸 base64；不支持本地路径——防任意文件读取）
	Image         json.RawMessage `json:"image"`
	InputFidelity string          `json:"input_fidelity"`
}

func (h *Handler) imageGenerations(w http.ResponseWriter, r *http.Request) {
	h.serveImage(w, r, false)
}

func (h *Handler) imageEdits(w http.ResponseWriter, r *http.Request) {
	h.serveImage(w, r, true)
}

func (h *Handler) serveImage(w http.ResponseWriter, r *http.Request, edit bool) {
	// edits 可携带 base64 图片，上限放宽到 32MB（chat 为 8MB）。
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	// 请求级统计：与 chat 同一条表格日志链路（任何出口都会落一行）。
	// 图像请求没有 token usage，tok/ctx/cache/think 列恒为 "-"，
	// 但请求数仍计入用量统计（Requests++、Missing++——用量页能看到
	// "这个图像模型被调了几次"，token 列如实为 0）。
	st := newChatStat(time.Now(), body, false)
	st.mode = "image"
	if edit {
		st.mode = "imgedit"
	}
	st.usageSink = h.cfg.UsageStore
	defer st.done()

	var in imageGenRequest
	if err := json.Unmarshal(body, &in); err != nil {
		st.status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		st.status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	if in.Size == "" {
		in.Size = "1024x1024"
	}
	if in.Model == "" {
		in.Model = defaultImageGenModel
		if edit {
			in.Model = defaultImageEditModel
		}
	}
	// 日志展示实际生效的模型（含默认补全），而非客户端原文字段。
	st.model = in.Model
	// 客户端未传 footnote 时注入网关默认（features.image_footnote 配置）；
	// 客户端显式传（含 ""）则以客户端为准。
	footnote := in.Footnote
	if footnote == nil && h.cfg.ImageFootnoteDefault != nil {
		footnote = h.cfg.ImageFootnoteDefault
	}
	req := upstream.ImageRequest{
		Model:         in.Model,
		Prompt:        in.Prompt,
		Size:          in.Size,
		N:             in.N,
		Footnote:      footnote,
		Revise:        in.Revise,
		Extra:         in.Extra,
		Quality:       in.Quality,
		Style:         in.Style,
		Background:    in.Background,
		InputFidelity: in.InputFidelity,
	}
	if edit {
		rawImgs, err := parseImageField(in.Image)
		if err != nil || len(rawImgs) == 0 {
			st.status = http.StatusBadRequest
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image is required (data URL / http(s) URL / base64)")
			return
		}
		// 归一化为 data URL 列表；远程图在入轮转前抓取一次即可（与账号无关，直连即可）。
		req.Images, err = h.cfg.Upstream.NormalizeImageInputs(nil, rawImgs)
		if err != nil {
			st.status = http.StatusBadRequest
			writeOpenAIError(w, http.StatusBadRequest, "invalid_image", err.Error())
			return
		}
	}

	tried := map[string]bool{}
	var lastErr error
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried, in.Model)
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.acct = acct
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}
		heldUID = acct.UID

		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				releaseHeld()
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("image refresh acct=%s: save auth failed: %v", logAccountName(acct), err)
			}
		}

		var items []upstream.ImageItem
		var proxy string
		var callErr error
		if edit {
			items, proxy, callErr = h.cfg.Upstream.EditImage(acct, req)
		} else {
			items, proxy, callErr = h.cfg.Upstream.GenerateImage(acct, req)
		}
		st.params.Proxy = proxy
		if callErr != nil {
			var ue *upstream.Error
			if errors.As(callErr, &ue) {
				st.status = ue.Status
				if st.status < 400 {
					// 信封错误（200 + code!=0）：HTTP 层是 200 但本次调用确已失败，
					// 状态列如实按网关语义记 502，不伪装成成功。
					st.status = http.StatusBadGateway
				}
				lastErr = ue
				h.applyErrorPolicy(acct, in.Model, ue.Kind, ue.Status, ue.Msg)
				log.Printf("image acct=%s model=%s edit=%v: %s", logAccountName(acct), in.Model, edit, ue)
			} else {
				// 传输层错误：换号不罚（与 chat 同策）。
				st.status = http.StatusServiceUnavailable
				lastErr = callErr
				log.Printf("image acct=%s model=%s edit=%v: transport error: %v", logAccountName(acct), in.Model, edit, callErr)
			}
			releaseHeld()
			continue
		}
		if len(items) == 0 {
			// 200 但无有效结果：上游返回了空 data——透传错误而非换号重试（与账号无关）。
			releaseHeld()
			st.status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_empty", "upstream returned no image")
			return
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		releaseHeld()
		// response_format=b64_json 兼容：hunyuan 族上游只回 url，客户端要 b64 时抓取内联。
		if strings.EqualFold(in.ResponseFormat, "b64_json") {
			h.cfg.Upstream.InlineImageURLs(acct, items)
		}
		st.status = http.StatusOK
		writeJSON(w, http.StatusOK, map[string]any{
			"created": time.Now().Unix(),
			"data":    items,
		})
		return
	}

	// 轮转耗尽：最后一个上游错误按原状态码透出（如 429 queue-full 客户端可自行退避）。
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		status := ue.Status
		if status < 400 {
			status = http.StatusBadGateway
		}
		st.status = status
		writeOpenAIError(w, status, ue.Kind.String(), ue.Msg)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	st.status = http.StatusServiceUnavailable
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// parseImageField 解析 edits 的 image 字段：字符串或字符串数组。
func parseImageField(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return list, nil
}
