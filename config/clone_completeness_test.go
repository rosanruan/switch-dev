package config

import (
	"reflect"
	"sync"
	"testing"

	"switchdev/proxy"
)

// TestCloneCoversAllFields 是 Clone 的「字段完整性守卫」：
// 用反射遍历 Config 的每个字段（除 mu/path），填充非零值后调用 Clone，
// 断言 Clone 产物对应字段也是非零——如果 Clone() 漏拷了某个字段，产物里该字段
// 一定是零值，测试立刻红，并报出具体字段名。
//
// 新增 Config 字段时若忘了更新 Clone()，这个测试会在 go test 时精准报错，
// 把 CLAUDE.md 里「改 Config 结构必须同步三处」的口头约定变成 CI 硬约束。
func TestCloneCoversAllFields(t *testing.T) {
	cfg := makeNonZeroConfig()
	clone := cfg.Clone()

	ct := reflect.TypeFor[Config]()
	cv := reflect.ValueOf(clone).Elem()

	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		// 跳过非序列化字段
		if f.Name == "mu" || f.Name == "path" {
			continue
		}
		fv := cv.FieldByName(f.Name)
		if !fv.IsValid() {
			t.Errorf("Clone 产物缺少字段 %s", f.Name)
			continue
		}
		if fv.IsZero() {
			t.Errorf("Clone 后字段 %s 为零值——Clone() 可能漏拷了该字段", f.Name)
		}
	}
}

// TestCloneDeepCopyIsolation 验证 Clone 对引用类型（slice/map）做的是深拷贝，
// 修改原对象不应影响 Clone 产物。
func TestCloneDeepCopyIsolation(t *testing.T) {
	cfg := makeNonZeroConfig()
	clone := cfg.Clone()

	// 修改原对象的 slice/map 字段
	cfg.AutoChain[0].Models[0] = "MUTATED"
	cfg.ManualFallbacks["claude-sonnet-4-5-20250514"][0].Model = "MUTATED"
	cfg.UARules[0].Pattern = "MUTATED"
	cfg.UARules[0].Mappings[0].RequestedModel = "MUTATED"
	cfg.Upstreams["test-upstream"] = UpstreamSettings{Enabled: false}
	cfg.Presets[0].Name = "MUTATED"
	cfg.Presets[0].AutoChain[0].Models[0] = "MUTATED"
	cfg.Presets[0].ManualFallbacks["gpt-4o"][0].Model = "MUTATED"
	cfg.Presets[0].UARules[0].Pattern = "MUTATED"

	// Clone 产物不应受影响
	if clone.AutoChain[0].Models[0] == "MUTATED" {
		t.Error("AutoChain 不是深拷贝：修改原对象影响了 Clone")
	}
	if clone.ManualFallbacks["claude-sonnet-4-5-20250514"][0].Model == "MUTATED" {
		t.Error("ManualFallbacks 不是深拷贝：修改原对象影响了 Clone")
	}
	if clone.UARules[0].Pattern == "MUTATED" {
		t.Error("UARules 不是深拷贝：修改原对象影响了 Clone")
	}
	if clone.UARules[0].Mappings[0].RequestedModel == "MUTATED" {
		t.Error("UARules.Mappings 不是深拷贝：修改原对象影响了 Clone")
	}
	if _, ok := clone.Upstreams["test-upstream"]; ok {
		t.Error("Upstreams 不是深拷贝：向原对象添加 key 出现在了 Clone 里")
	}
	if clone.Presets[0].Name == "MUTATED" {
		t.Error("Presets 不是深拷贝：修改原 Preset.Name 影响了 Clone")
	}
	if clone.Presets[0].AutoChain[0].Models[0] == "MUTATED" {
		t.Error("Preset.AutoChain 不是深拷贝：修改原对象影响了 Clone")
	}
	if clone.Presets[0].ManualFallbacks["gpt-4o"][0].Model == "MUTATED" {
		t.Error("Preset.ManualFallbacks 不是深拷贝：修改原对象影响了 Clone")
	}
	if clone.Presets[0].UARules[0].Pattern == "MUTATED" {
		t.Error("Preset.UARules 不是深拷贝：修改原对象影响了 Clone")
	}
}

// makeNonZeroConfig 返回一个所有字段都填了非零值的 Config，
// 用于 Clone 完整性和深拷贝测试。每加一个 Config 字段都应该在这里补上非零值。
func makeNonZeroConfig() *Config {
	return &Config{
		Mode: "auto",
		AutoChain: []AgentModels{
			{Upstream: "deveco", Models: []string{"glm-5.1", "doubao-1.5-pro"}},
		},
		ManualFallbacks: map[string][]proxy.ModelRef{
			"claude-sonnet-4-5-20250514": {{Upstream: "deveco", Model: "glm-5.1"}},
			"gpt-4o":                     {{Upstream: "joycode", Model: "gpt-4o"}},
		},
		GlobalFallback:   proxy.ModelRef{Upstream: "opencode", Model: "deepseek-v3"},
		UARoutingEnabled: true,
		UARules: []UARule{
			{
				ID:      "ua-1",
				Name:    "cursor-rule",
				Pattern: "Cursor",
				Enabled: true,
				Mappings: []UAModelMap{
					{RequestedModel: "claude-sonnet-4-5-20250514", Target: proxy.ModelRef{Upstream: "deveco", Model: "glm-5.1"}},
				},
				DefaultTarget: proxy.ModelRef{Upstream: "joycode", Model: "gpt-4o"},
			},
		},
		UAGlobalFallback: proxy.ModelRef{Upstream: "opencode", Model: "deepseek-v3"},
		Port:             8787,
		APIKey:           "sk-test-nonzero",
		AuthEnabled:      true,
		AutoStart:        true,
		AutoUpdate: UpdateConfig{
			Enabled:   true,
			Provider:  "github",
			GitHub:    GitHubConfig{Owner: "rosanruan", Repo: "switch-dev"},
			UpdateURL: "https://example.com/check",
			Channel:   "stable",
		},
		LogFile:      LogFileConfig{Enabled: true},
		ActivePreset: "test-preset",
		Provider:     ProviderSettings{AutoBenchmarkOnEdit: true, IdleAutoLock: true},
		Upstreams: map[string]UpstreamSettings{
			"deveco": {Enabled: true},
		},
		Presets: []Preset{
			{
				Name:    "test-preset",
				Mode:    "manual",
				AutoChain: []AgentModels{{Upstream: "joycode", Models: []string{"gpt-4o"}}},
				ManualFallbacks: map[string][]proxy.ModelRef{
					"gpt-4o": {{Upstream: "joycode", Model: "gpt-4o"}},
				},
				GlobalFallback:   proxy.ModelRef{Upstream: "opencode", Model: "deepseek-v3"},
				UARoutingEnabled: true,
				UARules: []UARule{
					{
						ID:      "ua-p1",
						Name:    "preset-rule",
						Pattern: "Windsurf",
						Enabled: true,
						Mappings: []UAModelMap{
							{RequestedModel: "gpt-4o", Target: proxy.ModelRef{Upstream: "joycode", Model: "gpt-4o"}},
						},
						DefaultTarget: proxy.ModelRef{Upstream: "joycode", Model: "gpt-4o"},
					},
				},
				UAGlobalFallback: proxy.ModelRef{Upstream: "opencode", Model: "deepseek-v3"},
			},
		},
		// mu 和 path 被跳过，不需要填
		mu:   sync.RWMutex{},
		path: "/tmp/test-config.json",
	}
}
