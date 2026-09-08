package proxy

import (
	"strings"
	"testing"

	"switchdev/upstream"
)

// TestMergeModelsDoesNotMutateInput MergeModels 过滤 opencode 非 free 模型时
// 不得原地改写调用方入参的底层数组（旧实现用 fetched[:0] 就会）。
func TestMergeModelsDoesNotMutateInput(t *testing.T) {
	input := []upstream.FetchedModel{
		{ID: "paid-model"},
		{ID: "mimo-v2.5-free"},
		{ID: "another-paid"},
	}
	original := make([]upstream.FetchedModel, len(input))
	copy(original, input)

	MergeModels("opencode", input, true)

	for i := range original {
		if input[i].ID != original[i].ID {
			t.Errorf("入参被改写：input[%d].ID = %q, 原为 %q", i, input[i].ID, original[i].ID)
		}
	}
}

// TestMergeModelsPreservesDevEcoWire DevEco 分支会把 mi.ID 从 wire 名覆写成
// 本地内部 id，Wire 字段必须保住 wire 名，否则路由发不出正确的 model。
func TestMergeModelsPreservesDevEcoWire(t *testing.T) {
	merged := MergeModels("deveco", []upstream.FetchedModel{
		{ID: "GLM-5.1", Label: "GLM-5.1", Context: 170000, Output: 131072},
	}, true)

	var found bool
	for _, m := range merged {
		if m.ID == "glm-5.1" {
			found = true
			if m.Wire != "GLM-5.1" {
				t.Errorf("Wire = %q, want GLM-5.1", m.Wire)
			}
		}
	}
	if !found {
		t.Fatalf("接口的 GLM-5.1 未映射到本地 id glm-5.1，merged=%+v", merged)
	}
}

// TestMergeModelsLiveOnlyKeepsRawID 种子里没有的实时模型保留原始 id，
// wire 名与 id 一致（自洽），不被静默改写成别的模型。
func TestMergeModelsLiveOnlyKeepsRawID(t *testing.T) {
	merged := MergeModels("deveco", []upstream.FetchedModel{
		{ID: "GLM-6.0", Label: "GLM-6.0", Context: 200000},
	}, true)

	for _, m := range merged {
		if m.ID == "auto" {
			continue
		}
		if m.ID != "GLM-6.0" {
			t.Errorf("live-only 模型 id 被改写为 %q, want GLM-6.0", m.ID)
		}
		if m.Wire != "GLM-6.0" {
			t.Errorf("Wire = %q, want GLM-6.0", m.Wire)
		}
	}
}

// TestLocalModelsCarryWire 拉取失败回退种子白名单时也要带上 wire 名
func TestLocalModelsCarryWire(t *testing.T) {
	cases := map[string]map[string]string{
		"deveco":    {"glm-5.1": "GLM-5.1"},
		"workbuddy": {"wb/glm-5.0": "glm-5.0"},
	}
	for upstreamName, want := range cases {
		for _, m := range MergeModels(upstreamName, nil, false) {
			if w, ok := want[m.ID]; ok && m.Wire != w {
				t.Errorf("%s/%s Wire = %q, want %q", upstreamName, m.ID, m.Wire, w)
			}
		}
	}
}

// TestAnthropicToOpenAIDevecoNoSilentRewrite 目录里查不到的 DevEco 模型必须原样透传，
// 不能像旧实现那样静默换成 DevEcoModels[0]（glm-5.1）。
func TestAnthropicToOpenAIDevecoNoSilentRewrite(t *testing.T) {
	SetCatalog(BuildCatalog(nil))

	got := AnthropicToOpenAIDeveco(&AnthropicRequest{Model: "GLM-9.9-unknown", MaxTokens: 1000})
	if got.Model != "GLM-9.9-unknown" {
		t.Errorf("未知 DevEco 模型被改写为 %q, want GLM-9.9-unknown", got.Model)
	}
	if got.MaxTokens != 1000 {
		t.Errorf("未知模型不该被钳制，MaxTokens = %d, want 1000", got.MaxTokens)
	}

	// 已知模型仍走 wire 名 + output 钳制
	known := AnthropicToOpenAIDeveco(&AnthropicRequest{Model: "glm-5.1", MaxTokens: 999999})
	if known.Model != "GLM-5.1" {
		t.Errorf("已知模型 Model = %q, want GLM-5.1", known.Model)
	}
	if known.MaxTokens != DevEcoModels[0].Output {
		t.Errorf("MaxTokens = %d, want %d", known.MaxTokens, DevEcoModels[0].Output)
	}
}

// TestLiveOnlyDevEcoWireEndToEnd 模拟 DB 回读后的目录，验证 live-only DevEco 模型
// 在两个入口（Anthropic 转换 + OpenAI 直通）都发出正确的 wire 名 —— 旧实现会静默改成 GLM-5.1。
func TestLiveOnlyDevEcoWireEndToEnd(t *testing.T) {
	defer SetCatalog(BuildCatalog(nil))
	SetCatalog(BuildCatalog([]CatalogModel{
		{ID: "glm-5.1", Upstream: "deveco", WireName: "GLM-5.1", Label: "GLM-5.1 (DevEco)", Context: 170000, Output: 131072},
		{ID: "GLM-6.0-live", Upstream: "deveco", WireName: "GLM-6.0-live", Label: "GLM-6.0 实时", Context: 222000, Output: 65536},
	}))

	got := AnthropicToOpenAIDeveco(&AnthropicRequest{Model: "GLM-6.0-live", MaxTokens: 999999})
	if got.Model != "GLM-6.0-live" {
		t.Errorf("Anthropic 入口 model = %q, want GLM-6.0-live", got.Model)
	}
	if got.MaxTokens != 65536 {
		t.Errorf("MaxTokens = %d, want 65536", got.MaxTokens)
	}

	s := &Server{}
	body, err := s.buildOpenAIPassthroughBody(
		map[string]interface{}{"model": "GLM-6.0-live"},
		ModelRef{Upstream: "deveco", Model: "GLM-6.0-live"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"model":"GLM-6.0-live"`) {
		t.Errorf("OpenAI 直通 body = %s", body)
	}
	body2, _ := s.buildOpenAIPassthroughBody(
		map[string]interface{}{"model": "glm-5.1"},
		ModelRef{Upstream: "deveco", Model: "glm-5.1"},
	)
	if !strings.Contains(string(body2), `"model":"GLM-5.1"`) {
		t.Errorf("种子模型 body = %s", body2)
	}
}
