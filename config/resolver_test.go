package config

import (
	"testing"

	"switchdev/proxy"
)

// newResolveConfig 构造一份直接用于 Resolve 测试的 Config（绕过 Manager/磁盘）
func newResolveConfig() *Config {
	c := Defaults()
	c.APIKey = "rs-test"
	return c
}

func refs(chain []proxy.ModelRef) [][2]string {
	out := make([][2]string, 0, len(chain))
	for _, r := range chain {
		out = append(out, [2]string{r.Upstream, r.Model})
	}
	return out
}

// TestAutoModeIgnoresRequestedModel 回归：auto 模式下客户端发具体模型名
// （如 Claude Code 发 claude-*），也必须走用户配置的 auto 链，
// 不能落到硬编码的 deveco/glm-5.1。
func TestAutoModeIgnoresRequestedModel(t *testing.T) {
	c := newResolveConfig()
	c.Mode = "auto"
	c.AutoChain = []AgentModels{{Upstream: "joycode", Models: []string{"JoyAI-Code-1.5"}}}

	for _, req := range []string{"claude-sonnet-4-5", "glm-5.1", "gpt-5", "auto", ""} {
		chain := c.Resolve(req, "test-agent")
		if len(chain) == 0 {
			t.Fatalf("Resolve(%q) 返回空链", req)
		}
		got := refs(chain)
		want := [2]string{"joycode", "JoyAI-Code-1.5"}
		if got[0] != want {
			t.Fatalf("Resolve(%q) 链首 = %v, 期望 %v（不应硬编码 glm-5.1）", req, got[0], want)
		}
	}
}

// TestManualModeKnownModel manual 模式：识别的模型以自身为链首 + 降级链 + 全局兜底
func TestManualModeKnownModel(t *testing.T) {
	c := newResolveConfig()
	c.Mode = "manual"
	c.ManualFallbacks = map[string][]proxy.ModelRef{
		"JoyAI-Code-1.5": {{Upstream: "deveco", Model: "glm-5.1"}},
	}
	c.GlobalFallback = proxy.ModelRef{Upstream: "opencode", Model: "mimo-v2.5-free"}

	chain := c.Resolve("JoyAI-Code-1.5", "test")
	got := refs(chain)
	want := [][2]string{
		{"joycode", "JoyAI-Code-1.5"},
		{"deveco", "glm-5.1"},
		{"opencode", "mimo-v2.5-free"},
	}
	if len(got) != len(want) {
		t.Fatalf("链长 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("链[%d] = %v, 期望 %v", i, got[i], want[i])
		}
	}
}

// TestManualModeUnknownModelFallsToGlobal manual 模式：未识别的模型名
// 不再硬编码落到 glm-5.1，而是用 GlobalFallback。
func TestManualModeUnknownModelFallsToGlobal(t *testing.T) {
	c := newResolveConfig()
	c.Mode = "manual"
	c.GlobalFallback = proxy.ModelRef{Upstream: "opencode", Model: "mimo-v2.5-free"}

	chain := c.Resolve("claude-sonnet-4-5", "test")
	got := refs(chain)
	want := [][2]string{{"opencode", "mimo-v2.5-free"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("未识别模型 Resolve = %v, 期望仅全局兜底 %v", got, want)
	}
}

// TestManualModeUnknownModelNoGlobal 未识别模型且无全局兜底时返回空链（不再编造 glm-5.1）
func TestManualModeUnknownModelNoGlobal(t *testing.T) {
	c := newResolveConfig()
	c.Mode = "manual"
	c.GlobalFallback = proxy.ModelRef{}

	chain := c.Resolve("claude-sonnet-4-5", "test")
	if len(chain) != 0 {
		t.Fatalf("无兜底时未识别模型应返回空链, 实际 %v", refs(chain))
	}
}

// TestIsUpstreamEnabledDefault 缺省（无配置）时所有上游视为启用，升级/新增供应商非破坏
func TestIsUpstreamEnabledDefault(t *testing.T) {
	c := newResolveConfig()
	for _, name := range []string{"joycode", "deveco", "opencode", "workbuddy", "groq", "custom-abc123"} {
		if !c.IsUpstreamEnabled(name) {
			t.Fatalf("缺省配置下 %s 应视为启用", name)
		}
	}
}

// TestIsUpstreamEnabledToggle 显式禁用返回 false，启用返回 true
func TestIsUpstreamEnabledToggle(t *testing.T) {
	c := newResolveConfig()
	c.Upstreams = map[string]UpstreamSettings{
		"deveco":  {Enabled: false},
		"joycode": {Enabled: true},
	}
	if c.IsUpstreamEnabled("deveco") {
		t.Fatal("deveco 已禁用，应返回 false")
	}
	if !c.IsUpstreamEnabled("joycode") {
		t.Fatal("joycode 已启用，应返回 true")
	}
	if !c.IsUpstreamEnabled("opencode") {
		t.Fatal("opencode 未配置，应默认启用")
	}
}

// TestCloneIsolatesUpstreams Clone 后改 Upstreams 不应串到原配置
func TestCloneIsolatesUpstreams(t *testing.T) {
	mgr := newTestManager(t)
	cfg := mgr.Get()
	cfg.Upstreams = map[string]UpstreamSettings{"deveco": {Enabled: false}}
	if err := mgr.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	c1 := mgr.Get()
	c1.Upstreams["deveco"] = UpstreamSettings{Enabled: true}
	c1.Upstreams["joycode"] = UpstreamSettings{Enabled: false}

	c2 := mgr.Get()
	if c2.Upstreams["deveco"].Enabled {
		t.Fatal("Clone 未隔离：改克隆的 Upstreams 影响了 Manager 持有配置")
	}
	if _, ok := c2.Upstreams["joycode"]; ok {
		t.Fatal("Clone 未隔离：新增 key 串到 Manager 持有配置")
	}
}

// TestUpstreamEnabledPersists 开关经 SaveConfig 持久化且热加载生效
func TestUpstreamEnabledPersists(t *testing.T) {
	mgr := newTestManager(t)
	cfg := mgr.Get()
	if cfg.Upstreams == nil {
		cfg.Upstreams = map[string]UpstreamSettings{}
	}
	cfg.Upstreams["workbuddy"] = UpstreamSettings{Enabled: false}
	if err := mgr.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if mgr.IsUpstreamEnabled("workbuddy") {
		t.Fatal("SaveConfig 后 IsUpstreamEnabled(workbuddy) 应为 false")
	}
	// 重新加载（模拟重启）
	mgr2, err := NewManager(mgr.config.path)
	if err != nil {
		t.Fatalf("NewManager reload: %v", err)
	}
	if mgr2.IsUpstreamEnabled("workbuddy") {
		t.Fatal("持久化失败：重载后 workbuddy 开关丢失")
	}
}

// TestUARuleDefaultTarget 路A：UA 命中但请求模型未命中任何 mapping 时，
// 整 UA 路由到规则级默认目标（cc-switch 式，免维护客户端模型清单）。
func TestUARuleDefaultTarget(t *testing.T) {
	c := newResolveConfig()
	c.Mode = "ua"
	c.UAGlobalFallback = proxy.ModelRef{Upstream: "workbuddy", Model: "wb/glm-5.0"}
	c.UARules = []UARule{
		{
			ID:      "ua-cc",
			Name:    "Claude Code",
			Pattern: "claude-cli",
			Enabled: true,
			// 只映射 opus，sonnet/haiku 走默认目标
			Mappings: []UAModelMap{
				{RequestedModel: "claude-opus-4-8", Target: proxy.ModelRef{Upstream: "joycode", Model: "JoyAI-Code-1.5"}},
			},
			DefaultTarget: proxy.ModelRef{Upstream: "deveco", Model: "glm-5.1"},
		},
	}

	// 1. 命中具体 mapping：优先用映射目标
	chain := c.Resolve("claude-opus-4-8", "claude-cli/1.0.0")
	got := refs(chain)
	if got[0] != [2]string{"joycode", "JoyAI-Code-1.5"} {
		t.Fatalf("应命中 mapping 目标 joycode/JoyAI-Code-1.5，实际 %v", got)
	}

	// 2. UA 命中但模型未命中 mapping：路由到规则默认目标，UA 全局兜底殿后
	chain = c.Resolve("claude-sonnet-4-6", "claude-cli/1.0.0")
	got = refs(chain)
	if got[0] != [2]string{"deveco", "glm-5.1"} {
		t.Fatalf("未命中 mapping 应走默认目标 deveco/glm-5.1，实际 %v", got)
	}
	if got[len(got)-1] != [2]string{"workbuddy", "wb/glm-5.0"} {
		t.Fatalf("UA 全局兜底应殿后，实际 %v", got)
	}

	// 3. 规则无默认目标：直接落到 UA 全局兜底
	c.UARules[0].DefaultTarget = proxy.ModelRef{}
	chain = c.Resolve("claude-sonnet-4-6", "claude-cli/1.0.0")
	got = refs(chain)
	if len(got) != 1 || got[0] != [2]string{"workbuddy", "wb/glm-5.0"} {
		t.Fatalf("无默认目标应只走 UA 全局兜底，实际 %v", got)
	}
}

// TestCloneIsolatesUARuleDefaultTarget Clone/copyUARules 必须拷贝 DefaultTarget，
// 否则编辑克隆会丢字段（Clone 是手写逐字段的）。
func TestCloneIsolatesUARuleDefaultTarget(t *testing.T) {
	c := newResolveConfig()
	c.UARules = []UARule{
		{
			ID:            "ua-cc",
			Pattern:       "claude-cli",
			Enabled:       true,
			DefaultTarget: proxy.ModelRef{Upstream: "deveco", Model: "glm-5.1"},
		},
	}
	cp := c.Clone()
	if cp.UARules[0].DefaultTarget.Upstream != "deveco" || cp.UARules[0].DefaultTarget.Model != "glm-5.1" {
		t.Fatalf("Clone 丢失 DefaultTarget: %+v", cp.UARules[0].DefaultTarget)
	}
	// 改克隆不影响原对象
	cp.UARules[0].DefaultTarget = proxy.ModelRef{Upstream: "joycode", Model: "JoyAI-Code-1.5"}
	if c.UARules[0].DefaultTarget.Upstream != "deveco" {
		t.Fatal("Clone 未隔离：改克隆的 DefaultTarget 影响了原配置")
	}
}
