package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadMergesNewDefaultRates 升级合并：已有 pricing.json 的用户，
// 内置库新增的模型（mcloud 渠道、GLM-5.3）必须补进来，否则永远不计费；
// 用户自定义/改过的条目不得被覆盖。
func TestLoadMergesNewDefaultRates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	existing := pricingFile{Version: 1, Prices: []*Price{
		{ModelID: "custom-x", DisplayName: "自定义", InputPerMillion: 1, OutputPerMillion: 2},
		{ModelID: "glm-5.2", DisplayName: "用户改过的 GLM-5.2", InputPerMillion: 9.99, OutputPerMillion: 9.99},
	}}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	m := NewManager()
	m.SetPath(path)
	imported, err := m.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if imported {
		t.Error("已有自有库时不应标记为从内置还原")
	}

	// 用户条目保留且不被覆盖
	if p := m.GetPrice("custom-x"); p == nil || p.OutputPerMillion != 2 {
		t.Errorf("自定义条目丢失: %+v", p)
	}
	if p := m.GetPrice("glm-5.2"); p == nil || p.InputPerMillion != 9.99 {
		t.Errorf("用户改过的默认条目被覆盖: %+v", p)
	}
	// 内置新增条目补入
	if p := m.GetPrice("huoshan_glm_5_3"); p == nil {
		t.Error("mcloud GLM-5.3 费率未合并进已有库")
	}
	if p := m.GetPrice("blian_qwen38_max"); p == nil {
		t.Error("mcloud Qwen3.8 Max 费率未合并进已有库")
	}

	// 合并结果已落盘（下次启动不再重复合并）
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f2 pricingFile
	if err := json.Unmarshal(data2, &f2); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range f2.Prices {
		if p.ModelID == "huoshan_glm_5_3" {
			found = true
		}
	}
	if !found {
		t.Error("合并后的自有库未持久化")
	}
}

// TestGLM53UppercaseLookup DevEco 的 wire 名是大小写混合的 GLM-5.3，
// 必须经 normalize 小写化命中 glm-5.3 费率（否则 DevEco 的 glm-5.3 也不计费）。
func TestGLM53UppercaseLookup(t *testing.T) {
	m := NewManager()
	m.SetPath(filepath.Join(t.TempDir(), "pricing.json"))
	if _, err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cost, price := m.CalculateCost("GLM-5.3", 1_000_000, 1_000_000)
	if price == nil {
		t.Fatal("GLM-5.3（大写 wire 名）查不到费率")
	}
	if cost < 5.8-1e-9 || cost > 5.8+1e-9 {
		t.Errorf("GLM-5.3 1M+1M 费用 = %v, want 5.8", cost)
	}
}

// TestMCloudModelsHaveRates mcloud 裸 modelType（= ActualUpstreamModel 的输出，
// 计费查找键）都能查到费率；九天无公开费率，刻意不收。
func TestMCloudModelsHaveRates(t *testing.T) {
	m := NewManager()
	m.SetPath(filepath.Join(t.TempDir(), "pricing.json"))
	if _, err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := map[string][2]float64{
		"huoshan_glm_5_3":      {1.4, 4.4},
		"blian_qwen38_max":     {2.5, 7.5},
		"blian_qwen37_plus":    {0.4, 1.6},
		"blian_deepseek_v4_pro": {0.435, 0.87},
	}
	for id, rates := range want {
		cost, price := m.CalculateCost(id, 1_000_000, 1_000_000)
		if price == nil {
			t.Errorf("%s 查不到费率", id)
			continue
		}
		if want := rates[0] + rates[1]; cost != want {
			t.Errorf("%s 1M+1M 费用 = %v, want %v", id, cost, want)
		}
	}
}
