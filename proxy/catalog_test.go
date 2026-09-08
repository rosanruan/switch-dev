package proxy

import (
	"sync"
	"testing"
)

// TestSeedCatalogMatchesLegacyResolution 目录未注入动态条目时，
// 解析行为必须与旧的静态 map 实现逐条一致（避免重构悄悄改路由）。
func TestSeedCatalogMatchesLegacyResolution(t *testing.T) {
	SetCatalog(BuildCatalog(nil))

	cases := []struct {
		requested    string
		wantResolved string
		wantUpstream string
		wantKnown    bool
	}{
		{"wb/glm-5.0", "wb/glm-5.0", "workbuddy", true},
		{"mimo-v2.5-free", "mimo-v2.5-free", "opencode", true},
		{"glm-5.1", "glm-5.1", "deveco", true},
		{"GLM-5.1 (DevEco)", "glm-5.1", "deveco", true}, // label 反查
		{"GLM-5.1", "glm-5.1", "deveco", true},          // DevEco wire 名反查
		{"JoyAI-Code-1.5", "JoyAI-Code-1.5", "joycode", true},
		{"MiniMax-M3", "MiniMax-M3-agent", "joycode", true}, // JoyCode label 反查
		{"nonexistent-model", "nonexistent-model", "joycode", false},
		{"", "", "joycode", false},
	}

	for _, c := range cases {
		if got := ResolveModel(c.requested); got != c.wantResolved {
			t.Errorf("ResolveModel(%q) = %q, want %q", c.requested, got, c.wantResolved)
		}
		if got := ResolveUpstream(c.requested); got != c.wantUpstream {
			t.Errorf("ResolveUpstream(%q) = %q, want %q", c.requested, got, c.wantUpstream)
		}
		if got := IsKnownModel(c.requested); got != c.wantKnown {
			t.Errorf("IsKnownModel(%q) = %v, want %v", c.requested, got, c.wantKnown)
		}
	}
}

// TestSeedWireNames 种子模型的 wire 名（发往上游 base_url 的真实 model）
func TestSeedWireNames(t *testing.T) {
	SetCatalog(BuildCatalog(nil))

	cases := map[string]string{
		"glm-5.1":        "GLM-5.1", // DevEco：内部 id 与 wire 名不同
		"wb/glm-5.0":     "glm-5.0", // WorkBuddy：剥 wb/ 前缀
		"mimo-v2.5-free": "mimo-v2.5-free",
		"JoyAI-Code-1.5": "JoyAI-Code-1.5",
		"unknown-xyz":    "unknown-xyz", // 目录里没有：原样
	}
	for id, want := range cases {
		if got := ActualUpstreamModel(id); got != want {
			t.Errorf("ActualUpstreamModel(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestLiveOnlyModelIsRoutable 只存在于实时拉取结果里的模型必须可识别、可路由，
// 且 wire 名自洽（这是「上游上了新模型不必等发版」的核心诉求）。
func TestLiveOnlyModelIsRoutable(t *testing.T) {
	defer SetCatalog(BuildCatalog(nil))
	SetCatalog(BuildCatalog([]CatalogModel{
		{ID: "GLM-6.0", Upstream: "deveco", WireName: "GLM-6.0", Label: "GLM-6.0", Context: 200000, Output: 65536},
		{ID: "brand-new-free", Upstream: "opencode", Label: "Brand New Free", Output: 8000},
	}))

	if !IsKnownModel("GLM-6.0") {
		t.Error("live-only DevEco 模型应被识别")
	}
	if got := ResolveUpstream("GLM-6.0"); got != "deveco" {
		t.Errorf("ResolveUpstream(GLM-6.0) = %q, want deveco", got)
	}
	if got := ActualUpstreamModel("GLM-6.0"); got != "GLM-6.0" {
		t.Errorf("ActualUpstreamModel(GLM-6.0) = %q, want GLM-6.0", got)
	}
	if got := modelContextLimit("GLM-6.0"); got != 200000 {
		t.Errorf("modelContextLimit(GLM-6.0) = %d, want 200000", got)
	}
	// WireName 留空时按上游规则推导
	if got := ActualUpstreamModel("brand-new-free"); got != "brand-new-free" {
		t.Errorf("ActualUpstreamModel(brand-new-free) = %q", got)
	}
	// 种子模型不能被动态条目挤掉
	if got := ResolveUpstream("glm-5.1"); got != "deveco" {
		t.Errorf("种子模型 glm-5.1 丢失，ResolveUpstream = %q", got)
	}
}

// TestLiveOverridesSeedMetadata 同上游同 id：实时元数据覆盖种子（含改小）
func TestLiveOverridesSeedMetadata(t *testing.T) {
	defer SetCatalog(BuildCatalog(nil))
	SetCatalog(BuildCatalog([]CatalogModel{
		{ID: "glm-5.1", Upstream: "deveco", WireName: "GLM-5.1", Label: "GLM-5.1 新", Context: 99, Output: 88},
	}))

	m, ok := LookupModel("glm-5.1")
	if !ok {
		t.Fatal("glm-5.1 应在目录中")
	}
	if m.Context != 99 || m.Output != 88 {
		t.Errorf("实时元数据未覆盖种子：context=%d output=%d，want 99/88", m.Context, m.Output)
	}
	if !m.Live {
		t.Error("动态条目应标记 Live")
	}
}

// TestDynamicEntryCannotHijackOtherUpstream 动态条目不能把别的上游的种子模型抢过来
func TestDynamicEntryCannotHijackOtherUpstream(t *testing.T) {
	defer SetCatalog(BuildCatalog(nil))
	SetCatalog(BuildCatalog([]CatalogModel{
		{ID: "glm-5.1", Upstream: "joycode", Label: "冒名"},
	}))

	if got := ResolveUpstream("glm-5.1"); got != "deveco" {
		t.Errorf("glm-5.1 被 joycode 劫持，ResolveUpstream = %q, want deveco", got)
	}
}

// TestCatalogSnapshotCoversAllSeeds 快照条目数 = 四个种子白名单之和
func TestCatalogSnapshotCoversAllSeeds(t *testing.T) {
	SetCatalog(BuildCatalog(nil))
	want := len(JoyCodeModels) + len(DevEcoModels) + len(OpenCodeModels) + len(WorkBuddyModels)
	if got := len(CatalogSnapshot()); got != want {
		t.Errorf("CatalogSnapshot() 有 %d 条，want %d", got, want)
	}
	for _, up := range []string{"joycode", "deveco", "opencode", "workbuddy"} {
		if len(CatalogModelsByUpstream(up)) == 0 {
			t.Errorf("上游 %s 在目录中为空", up)
		}
	}
}

// TestAliasNeverShadowsRealID 别名表不得遮蔽真实 id
func TestAliasNeverShadowsRealID(t *testing.T) {
	SetCatalog(BuildCatalog(nil))
	c := currentCatalog()
	for alias := range c.aliases {
		if _, isRealID := c.byID[alias]; isRealID {
			t.Errorf("别名 %q 与真实 id 冲突", alias)
		}
	}
}

// TestProviderModelsRoundTrip provider 模型经原子指针发布
func TestProviderModelsRoundTrip(t *testing.T) {
	defer SetProviderModels(nil)
	SetProviderModels([]ProviderModel{
		{InternalID: "provider/groq/llama-4", ProviderID: "groq", ModelID: "llama-4", Label: "Llama 4"},
	})
	list := ProviderModelList()
	if len(list) != 1 || list[0].ProviderID != "groq" {
		t.Fatalf("ProviderModelList() = %+v", list)
	}
	// provider 模型走前缀路由，不查目录
	if got := ResolveUpstream("provider/groq/llama-4"); got != "groq" {
		t.Errorf("ResolveUpstream = %q, want groq", got)
	}
	if got := ActualUpstreamModel("provider/groq/llama-4"); got != "llama-4" {
		t.Errorf("ActualUpstreamModel = %q, want llama-4", got)
	}
}

// TestCatalogConcurrentAccess 目录快照替换与热路径读并发（配 -race 跑）
func TestCatalogConcurrentAccess(t *testing.T) {
	defer SetCatalog(BuildCatalog(nil))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ResolveModel("glm-5.1")
					ResolveUpstream("wb/glm-5.0")
					IsKnownModel("GLM-5.1")
					ActualUpstreamModel("glm-5.1")
					modelContextLimit("mimo-v2.5-free")
					_ = CatalogSnapshot()
					_ = CatalogModelsByUpstream("deveco")
					_ = ProviderModelList()
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		SetCatalog(BuildCatalog([]CatalogModel{
			{ID: "live-model", Upstream: "deveco", Label: "Live"},
		}))
		SetProviderModels([]ProviderModel{{InternalID: "provider/p/m", ProviderID: "p", ModelID: "m"}})
		SetCatalog(BuildCatalog(nil))
		SetProviderModels(nil)
	}
	close(stop)
	wg.Wait()
}
