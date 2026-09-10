package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// resetModelsCache 清空动态模型缓存并在测试结束恢复，避免污染其它用例。
func resetModelsCache(t *testing.T) {
	t.Helper()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	})
}

// TestModelListExtendedFields 验证 /v1/models 透出思考深度等扩展参数。
//
// 这些字段来自上游 models 接口（reasoning.defaultEffort / supportedEfforts /
// canDisableThinking、能力标志、消耗倍率），此前只暴露 id/context_length/max_output_tokens。
func TestModelListExtendedFields(t *testing.T) {
	resetModelsCache(t)

	temp := 0.7
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{
			ID: "glm-5.3", Name: "GLM-5.3",
			ContextWindow: 1000000, MaxTokens: 48000,
			DefaultEffort: "high", Efforts: []string{"low", "high", "max"},
			CanDisableThinking: true, OnlyReasoning: true,
			SupportsImages: true, SupportsToolCall: true, SupportsReasoning: true,
			CreditsRate: 0.79, CreditsText: "x0.79", DescriptionZh: "能力均衡",
			Temperature: &temp,
		},
		{
			ID: "deepseek-v4-pro", Name: "Deepseek-V4-Pro",
			ContextWindow: 1000000, MaxTokens: 50000,
			DefaultEffort: "high", CanDisableThinking: false,
			CreditsRate: 0.51, CreditsText: "x0.51 credits",
		},
	}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("models=%d want 2", len(resp.Data))
	}

	byID := map[string]map[string]any{}
	for _, m := range resp.Data {
		byID[m["id"].(string)] = m
	}

	// ── 可调档模型：应有 supported_efforts 与 can_disable_thinking ──
	g := byID["glm-5.3"]
	if g == nil {
		t.Fatal("缺 glm-5.3")
	}
	r, ok := g["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("glm-5.3 缺 reasoning 字段: %v", g)
	}
	if r["default_effort"] != "high" {
		t.Errorf("default_effort=%v want high", r["default_effort"])
	}
	se, ok := r["supported_efforts"].([]any)
	if !ok || len(se) != 3 {
		t.Errorf("supported_efforts=%v want 3 项", r["supported_efforts"])
	}
	if r["can_disable_thinking"] != true {
		t.Errorf("can_disable_thinking=%v want true", r["can_disable_thinking"])
	}
	if g["credits_multiplier"] != 0.79 {
		t.Errorf("credits_multiplier=%v want 0.79", g["credits_multiplier"])
	}
	if g["description"] != "能力均衡" {
		t.Errorf("description=%v", g["description"])
	}
	if caps, ok := g["capabilities"].(map[string]any); !ok || caps["images"] != true {
		t.Errorf("capabilities=%v want images=true", g["capabilities"])
	}
	// OpenAI 标准字段必须保留（不能因扩展而破坏兼容）
	if g["object"] != "model" || g["owned_by"] != "workbuddy" {
		t.Errorf("标准字段被破坏: %v", g)
	}
	if g["context_length"] != float64(1000000) {
		t.Errorf("context_length=%v", g["context_length"])
	}

	// ── 固定单档模型：有 default_effort，不应有 supported_efforts ──
	d := byID["deepseek-v4-pro"]
	if d == nil {
		t.Fatal("缺 deepseek-v4-pro")
	}
	dr, ok := d["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("deepseek-v4-pro 缺 reasoning: %v", d)
	}
	if _, has := dr["supported_efforts"]; has {
		t.Error("固定单档模型不应出现 supported_efforts")
	}
	if dr["default_effort"] != "high" {
		t.Errorf("default_effort=%v want high", dr["default_effort"])
	}
}

// TestModelListFallsBackToStatic 验证动态拉取失败时仍回落静态表（既有行为不变）。
func TestModelListFallsBackToStatic(t *testing.T) {
	resetModelsCache(t)
	// 无账号 → Pick 返回 nil → fetchDynamicModels 返回 nil → 回落静态表
	h := NewHandler(Config{Pool: testPoolWith()})
	list := h.modelList()
	if len(list) == 0 {
		t.Fatal("应回落静态模型表")
	}
	if list[0]["id"] == nil {
		t.Errorf("静态表项缺 id: %v", list[0])
	}
}
