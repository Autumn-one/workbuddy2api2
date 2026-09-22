package upstream

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// catalogFixture 模拟上游目录：cli 组含正常/禁用成员，另有 text-to-image
// 与归属其他分组的条目，验证 FetchCatalog 双视图的拆分与标注。
func catalogFixture() string {
	return `{"code":0,"data":{
		"models":[
			{"id":"m-a","name":"A","maxInputTokens":1000,"maxOutputTokens":500,"supportsToolCall":true},
			{"id":"m-b","name":"B","maxInputTokens":1000,"maxOutputTokens":500,"disabled":true},
			{"id":"img-1","name":"Img","maxOutputTokens":0,"tags":["text-to-image"]},
			{"id":"kimi-k3-1","name":"K3","maxInputTokens":1000,"maxOutputTokens":500},
			{"id":"other","name":"O","maxInputTokens":1000,"maxOutputTokens":500}
		],
		"agents":[
			{"name":"cli","models":["m-a","m-b","kimi-k3-1"]},
			{"name":"Explore","models":["other"]}
		]}}`
}

func fetchCatalogFixture(t *testing.T) (*Client, *auth.Auth) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, catalogFixture()), nil
	})
	return c, &auth.Auth{AccessToken: "at", UID: "u1"}
}

func ids(infos []ModelInfo) []string {
	out := make([]string, 0, len(infos))
	for _, mi := range infos {
		out = append(out, mi.ID)
	}
	return out
}

func TestFetchCatalogCLIFiltersDisabledAndSynthesizesAlias(t *testing.T) {
	c, a := fetchCatalogFixture(t)
	cli, all, err := c.FetchCatalog(a)
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	// cli 视图：m-b 被 disabled 滤掉；kimi-k3-1 触发别名 kimi-k3 补录
	want := []string{"m-a", "kimi-k3-1", "kimi-k3"}
	got := ids(cli)
	if len(got) != len(want) {
		t.Fatalf("cli=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cli=%v want %v", got, want)
		}
	}
	// cli 条目 Agents 必含 "cli"
	for _, mi := range cli {
		found := false
		for _, g := range mi.Agents {
			if g == "cli" {
				found = true
			}
		}
		if !found {
			t.Errorf("cli 条目 %s Agents=%v 缺 cli", mi.ID, mi.Agents)
		}
	}
	_ = all
}

func TestFetchCatalogAllIncludesNonCLIAndDisabled(t *testing.T) {
	c, a := fetchCatalogFixture(t)
	_, all, err := c.FetchCatalog(a)
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	// all 视图：cli 实成员在前（声明顺序），其余按目录原序；无别名 kimi-k3
	want := []string{"m-a", "kimi-k3-1", "m-b", "img-1", "other"}
	got := ids(all)
	if len(got) != len(want) {
		t.Fatalf("all=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("all=%v want %v", got, want)
		}
	}
	byID := map[string]ModelInfo{}
	for _, mi := range all {
		byID[mi.ID] = mi
	}
	// m-b：cli 组成员但 disabled —— 必须出现在 all 且带标记
	if !byID["m-b"].Disabled {
		t.Errorf("m-b 应标 Disabled")
	}
	if len(byID["m-b"].Agents) != 1 || byID["m-b"].Agents[0] != "cli" {
		t.Errorf("m-b Agents=%v want [cli]", byID["m-b"].Agents)
	}
	// img-1：text-to-image、不在任何分组
	if len(byID["img-1"].Agents) != 0 {
		t.Errorf("img-1 Agents=%v want 空", byID["img-1"].Agents)
	}
	// other：非 cli 分组归属正确标注
	if len(byID["other"].Agents) != 1 || byID["other"].Agents[0] != "Explore" {
		t.Errorf("other Agents=%v want [Explore]", byID["other"].Agents)
	}
	// all 里不得出现本地合成别名
	if _, ok := byID["kimi-k3"]; ok {
		t.Errorf("all 不应含别名 kimi-k3")
	}
}

// TestFetchModelsStillCLIOnly 确认旧接口口径不变：只回 cli 可对话集合。
func TestFetchModelsStillCLIOnly(t *testing.T) {
	c, a := fetchCatalogFixture(t)
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	for _, mi := range infos {
		if mi.Disabled {
			t.Errorf("FetchModels 返回了禁用条目 %s", mi.ID)
		}
	}
	// 生图模型与非 cli 模型不得混入
	for _, mi := range infos {
		if mi.ID == "img-1" || mi.ID == "other" || mi.ID == "m-b" {
			t.Errorf("FetchModels 混入非可对话条目 %s", mi.ID)
		}
	}
}
